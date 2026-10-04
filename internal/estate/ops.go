package estate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The serving half: what a data node answers with.
//
// Declared here, by the consumer, and kept to exactly what the operations
// below call — the seat surface's tools, and the operator's own surfaces: the
// dashboard and REST routes and the operator's MCP. Those reach the estate
// through this node's router as a seat's tools do, so a node whose copy is out
// of service answers its operator from a peer's copy rather than its own, and a
// node holding no data serves them at all.

// TrackerReader is the tracker's read side the surfaces ask of.
type TrackerReader interface {
	Tasks(ctx context.Context, q tracker.Query, now time.Time) (tracker.Answer, error)
	Task(ctx context.Context, idOrKey string, want tracker.DetailWants,
		fresh statelog.Freshness) (tracker.TaskDetail, error)
	Views(ctx context.Context, q tracker.ViewQuery) (tracker.ViewListing, error)
	ExpandedQuery(ctx context.Context, params map[string]any,
		viewer tracker.Viewer, now time.Time, loc *time.Location) (tracker.Query, error)
	Catalogue(ctx context.Context, q tracker.CatalogueQuery) (tracker.CatalogueAnswer, error)
	Person(ctx context.Context, q tracker.PersonQuery, now time.Time) (tracker.PersonState, error)
	Thread(ctx context.Context, q tracker.ThreadQuery,
		fresh statelog.Freshness) (tracker.ResolvedThread, error)
	Activity(ctx context.Context, q tracker.ActivityQuery, now time.Time) (
		tracker.ActivityAnswer, error)
	MyWork(ctx context.Context, q tracker.MyWorkQuery, now time.Time,
		loc *time.Location) (tracker.MyWork, error)
	Projects(ctx context.Context, q tracker.ProjectQuery) (tracker.ProjectListing, error)
	Project(ctx context.Context, q tracker.ProjectDetailQuery) (tracker.ProjectDetail, error)
	Files(ctx context.Context, q tracker.FileQuery) (tracker.FileListing, error)
	File(ctx context.Context, project, path string, fresh statelog.Freshness) (tracker.FileDetail, error)

	// The operator's reads: a unit's workload, a person's inbox and the
	// routing a change took.
	Workload(ctx context.Context, q tracker.WorkloadQuery, now time.Time,
		loc *time.Location) (tracker.WorkloadAnswer, error)
	Inbox(ctx context.Context, q tracker.InboxQuery, now time.Time) (tracker.InboxAnswer, error)
	Routing(ctx context.Context, q tracker.RoutingQuery, now time.Time) (tracker.RoutingAnswer, error)
	TurnsOf(ctx context.Context, idOrKey, cursor string, limit int,
		fresh statelog.Freshness) (tracker.TaskTurns, error)
	EveryView(ctx context.Context, q tracker.EveryViewQuery) (tracker.ViewListing, error)
	Flow(ctx context.Context, q tracker.FlowQuery, now time.Time,
		loc *time.Location) (tracker.FlowAnswer, error)
	CompanyFeed(ctx context.Context, q tracker.FeedQuery) (tracker.FeedPage, error)
	Decisions(ctx context.Context, q tracker.DecisionsQuery, now time.Time,
		loc *time.Location) (tracker.DecisionsAnswer, error)
	TurnPlaces(ctx context.Context, runs []string,
		fresh statelog.Freshness) (map[string]tracker.TurnPlace, error)
}

// TrackerWriter is the tracker's write side, as ONE actor — see [Actor].
type TrackerWriter interface {
	CreateTask(ctx context.Context, opID string, task tracker.Task,
		notify *tracker.Notify) (tracker.WriteResult, error)
	CreateTaskAsking(ctx context.Context, opID string, task tracker.Task,
		ask tracker.Comment, notify *tracker.Notify) (tracker.WriteResult, error)
	PlaceTask(ctx context.Context, opID string, place tracker.Place,
		notify *tracker.Notify) (tracker.PlaceResult, error)
	UpdateTask(ctx context.Context, opID, id, project string, ifMatch uint64,
		patch tracker.TaskPatch, kind tracker.ChangeKind,
		notify *tracker.Notify) (tracker.WriteResult, error)
	Depend(ctx context.Context, opID string, change tracker.DependencyChange,
		leads tracker.Leads) (tracker.DependencyResult, error)
	MergeDuplicates(ctx context.Context, opID, duplicate, into string,
		reparent bool, notify *tracker.Notify) (tracker.WriteResult, error)
	MoveTaskToProject(ctx context.Context, opID, taskID, target string,
		notify *tracker.Notify) (tracker.WriteResult, error)
	WriteProject(ctx context.Context, opID, key string, edit tracker.ProjectEdit,
		authority tracker.ProjectAuthority) (tracker.WriteResult, error)
	WriteTags(ctx context.Context, opID, project string, edit tracker.TagEdit,
		authority tracker.TagAuthority) (tracker.WriteResult, error)
	EnsureTags(ctx context.Context, opID, project string, tags []string) (
		[]string, []string, error)
	PutFile(ctx context.Context, opID string, put tracker.FilePut) (tracker.WriteResult, error)
	RemoveFile(ctx context.Context, opID, project, path string, ifMatch uint64) (tracker.WriteResult, error)
	RecordTurn(ctx context.Context, opID string, turn tracker.TurnRecord) (tracker.WriteResult, error)

	// The operator's writes, which no seat is handed: saved views, the
	// catalogue, a person's own state, the trash and the purge.
	WriteView(ctx context.Context, opID string, view tracker.View) (tracker.WriteResult, error)
	WriteTypes(ctx context.Context, opID string, types []tracker.TaskType) (tracker.WriteResult, error)
	WriteFields(ctx context.Context, opID string, fields []tracker.FieldDef) (tracker.WriteResult, error)
	MarkInbox(ctx context.Context, opID, handle string,
		gesture tracker.InboxGesture) (tracker.WriteResult, error)
	WritePins(ctx context.Context, opID, handle string,
		gesture tracker.PinGesture) (tracker.WriteResult, error)
	WritePriorities(ctx context.Context, opID, handle string, priorities []string,
		ifMatch *uint64, authority tracker.PersonAuthority) (tracker.WriteResult, error)
	RemoveTask(ctx context.Context, opID, id, project string, subtree bool,
		notify *tracker.Notify) (tracker.WriteResult, error)
	RestoreTask(ctx context.Context, opID, id, project string,
		notify *tracker.Notify) (tracker.WriteResult, error)
	PurgeTask(ctx context.Context, opID, id, project, reason string) (tracker.WriteResult, error)
}

// WorkSearcher is the tracker's ranked search over this node's corpus.
type WorkSearcher interface {
	Search(ctx context.Context, q tracker.SearchQuery) (tracker.SearchAnswer, error)
}

// PageReader is the knowledge base's read side.
type PageReader interface {
	List(ctx context.Context, f pages.Filter, fresh statelog.Freshness) (pages.Listing, error)
	Get(ctx context.Context, ref string, fresh statelog.Freshness) (pages.Detail, error)
	SkillPages(ctx context.Context, container string, fresh statelog.Freshness) ([]pages.Page, error)

	// The operator's reads: every container, what happened to a page or
	// a container, and one revision of a page's body.
	Containers(ctx context.Context, fresh statelog.Freshness) ([]pages.ContainerListing, error)
	Activity(ctx context.Context, q pages.PageActivityQuery) (pages.PageActivity, error)
	Revision(ctx context.Context, pageID string, version int,
		fresh statelog.Freshness) (pages.Revision, bool, error)
}

// PageWriter is the knowledge base's write side. The author is an argument
// of every call rather than a derived writer, which is the pages store's own
// shape.
type PageWriter interface {
	Create(ctx context.Context, actor pages.Actor, in pages.NewPage) (pages.Written, error)
	SavePage(ctx context.Context, actor pages.Actor, pageID string, save pages.Save) (pages.Written, error)
	Rename(ctx context.Context, actor pages.Actor, pageID, title string, quiet bool,
		key pages.CallKey) (pages.Written, error)
	Comment(ctx context.Context, actor pages.Actor, pageID string,
		in pages.NewComment) (pages.Comment, pages.Written, error)
	EditComment(ctx context.Context, actor pages.Actor, pageID, commentID,
		body string, key pages.CallKey) (pages.Comment, pages.Written, error)
}

// KnowledgeSearcher is the native knowledge search over this node's corpus,
// with the one question a caller asks of an empty answer.
//
// ANSWER, NOT THE BEST-EFFORT SEARCH: a search that could not run is an error
// here ([pages.Searcher.Answer]), because the asking node has to tell "the
// knowledge base could not be searched" from "nothing matched" — and an empty
// answer carried back over the wire reads as the second.
type KnowledgeSearcher interface {
	Answer(ctx context.Context, q knowledge.Query) (knowledge.Result, error)
	Building(ctx context.Context) bool
}

// Backend is one data node's answer to every operation, over its copy of the
// estate, resolved PER REQUEST: a node that booted with no company brings its
// native runtime up at its first apply, and a request that arrives before then
// is answered "not here" rather than refused for ever.
//
// A nil half is a half this node does not run, and an operation on it is
// answered [unservedNoBackend] — the company is on a vendor for it.
type Backend struct {
	Tracker    TrackerReader
	Writer     func(Actor) TrackerWriter
	WorkSearch WorkSearcher
	Pages      PageReader
	PageWriter PageWriter
	Knowledge  KnowledgeSearcher

	// Committed waits until this node's applier of the floor's log holds a
	// position — the floor every request carries, the node's own included.
	Committed func(ctx context.Context, at statelog.Position) error

	// Answers is whether this copy may answer a request now
	// ([statelog.Health.Answers]): serving — drained since its appliers
	// started and within the snapshot slack of every log's end — or level
	// this instant. A copy that does not LAGS: it is asked only once every
	// data node whose copy does not has run nothing, by a remote asker and
	// by this node's own router alike — a worse node to ask, never no node
	// ([Router.route]).
	//
	// NOT A SEAT'S ADMISSION, which refuses at a lag of one: a busy
	// company has a record in flight on most instants, and a request gate
	// that refused then sent every read — this node's own seats' included —
	// to whichever peer happened to be level, or to none. What a read must
	// see of its own node's writes is what the floors carry.
	Answers func(ctx context.Context) bool

	// Admits is whether a SEAT may attach to this copy now: established at
	// a lag of zero — the gate every node's seat admission asks of the copy
	// that will serve it ([Router.Serves]). Only admission's ping reads it.
	Admits func(ctx context.Context) bool

	// ServerSeams are the node's own: the dispatcher sets them on every
	// backend it hands an operation.
	ServerSeams
}

// ServerSeams are what an operation reads of the SERVING node rather than of
// its copy: the chart seams a query or a dependency consults and the org a
// knowledge search is scoped by — the node's current epoch, which is what the
// package doc says crosses no wire — and the node's own event log.
type ServerSeams struct {
	// Units is attached to every tracker read that renders a project's
	// unit, and Leads is handed to a dependency.
	Units tracker.Units
	Leads tracker.Leads

	// Seat resolves a searching seat's handle to its role and the org its
	// read scope comes from, against this node's current epoch.
	Seat func(handle string) (*org.Role, *org.Organization)
}

// errNoHalf is an operation on a half this node does not run.
var errNoHalf = errors.New("estate: this node runs no native backend for this operation")

// errNotAdmitting is admission's ping on a copy that admits no seat yet
// ([Backend.Admits]): nothing ran, and the asker moves on.
var errNotAdmitting = errors.New("estate: this copy admits no seat yet")

// The domains an operation's floor may be on, and each one's log — named as it
// has always been, the one log the estate keeps of the domain. An operation's
// floor is on its OWN domain's log and no other: see [ready].
var (
	trackerDomain = tracker.Domain{}.Name()
	pagesDomain   = pages.Domain{}.Name()

	floorStreams = map[string]string{
		trackerDomain: statelog.EstateStream(tracker.Domain{}).Name,
		pagesDomain:   statelog.EstateStream(pages.Domain{}).Name,
	}
)

// ---- the tracker's reads ------------------------------------------------ //

type tasksArgs struct {
	Query tracker.Query
	Now   time.Time
}

var opTasks = define("tracker.tasks", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a tasksArgs) (tracker.Answer, error) {
		if b.Tracker == nil {
			return tracker.Answer{}, errNoHalf
		}
		a.Query.Units = b.Units
		return b.Tracker.Tasks(ctx, a.Query, a.Now)
	})

type taskArgs struct {
	IDOrKey string
	Want    tracker.DetailWants
	Fresh   statelog.Freshness

	// Zone is the name of the zone the asker's day clock is on
	// ([tracker.DayClock.Zone], [zoneArg]).
	Zone string
}

var opTask = define("tracker.task", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a taskArgs) (tracker.TaskDetail, error) {
		if b.Tracker == nil {
			return tracker.TaskDetail{}, errNoHalf
		}
		if a.Want.Clock != nil {
			loc, err := zoneOf(a.Zone)
			if err != nil {
				return tracker.TaskDetail{}, err
			}
			clock := *a.Want.Clock
			clock.Zone = loc
			a.Want.Clock = &clock
		}
		a.Want.Units = b.Units
		return b.Tracker.Task(ctx, a.IDOrKey, a.Want, a.Fresh)
	})

// viewsArgs is a views query as it crosses, with the zone its pinned counts
// cut relative dates on carried by name ([zoneArg]).
type viewsArgs struct {
	Query tracker.ViewQuery
	Zone  string
}

var opViews = define("tracker.views", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a viewsArgs) (tracker.ViewListing, error) {
		if b.Tracker == nil {
			return tracker.ViewListing{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return tracker.ViewListing{}, err
		}
		q := a.Query
		q.Units, q.Zone = b.Units, loc
		return b.Tracker.Views(ctx, q)
	})

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
	Viewer tracker.Viewer
	Now    time.Time

	// Zone is the location's name ([zoneArg]).
	Zone string
}

var opExpandedQuery = define("tracker.expanded_query", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a expandArgs) (tracker.Query, error) {
		if b.Tracker == nil {
			return tracker.Query{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return tracker.Query{}, err
		}
		return b.Tracker.ExpandedQuery(ctx, a.Params, a.Viewer, a.Now, loc)
	})

var opCatalogue = define("tracker.catalogue", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q tracker.CatalogueQuery) (tracker.CatalogueAnswer, error) {
		if b.Tracker == nil {
			return tracker.CatalogueAnswer{}, errNoHalf
		}
		return b.Tracker.Catalogue(ctx, q)
	})

type personArgs struct {
	Query tracker.PersonQuery
	Now   time.Time
}

var opPerson = define("tracker.person", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a personArgs) (tracker.PersonState, error) {
		if b.Tracker == nil {
			return tracker.PersonState{}, errNoHalf
		}
		return b.Tracker.Person(ctx, a.Query, a.Now)
	})

type threadArgs struct {
	Query tracker.ThreadQuery
	Fresh statelog.Freshness
}

var opThread = define("tracker.thread", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a threadArgs) (tracker.ResolvedThread, error) {
		if b.Tracker == nil {
			return tracker.ResolvedThread{}, errNoHalf
		}
		return b.Tracker.Thread(ctx, a.Query, a.Fresh)
	})

type activityArgs struct {
	Query tracker.ActivityQuery
	Now   time.Time
}

var opActivity = define("tracker.activity", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a activityArgs) (tracker.ActivityAnswer, error) {
		if b.Tracker == nil {
			return tracker.ActivityAnswer{}, errNoHalf
		}
		return b.Tracker.Activity(ctx, a.Query, a.Now)
	})

type myWorkArgs struct {
	Query tracker.MyWorkQuery
	Now   time.Time

	// Zone is the company clock the asker cut "today" on ([zoneArg]).
	Zone string
}

var opMyWork = define("tracker.my_work", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a myWorkArgs) (tracker.MyWork, error) {
		if b.Tracker == nil {
			return tracker.MyWork{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return tracker.MyWork{}, err
		}
		return b.Tracker.MyWork(ctx, a.Query, a.Now, loc)
	})

var opProjects = define("tracker.projects", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q tracker.ProjectQuery) (tracker.ProjectListing, error) {
		if b.Tracker == nil {
			return tracker.ProjectListing{}, errNoHalf
		}
		q.Units = b.Units
		return b.Tracker.Projects(ctx, q)
	})

var opProject = define("tracker.project", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q tracker.ProjectDetailQuery) (tracker.ProjectDetail, error) {
		if b.Tracker == nil {
			return tracker.ProjectDetail{}, errNoHalf
		}
		q.Units = b.Units
		return b.Tracker.Project(ctx, q)
	})

type workSearchArgs struct {
	Text  string
	Limit int

	// Mode is how to rank. EMPTY FROM AN OLDER BUILD, which ranked hybrid
	// and is answered as it always was: the zero value is hybrid.
	Mode knowledge.Mode
}

// THE RANKED SEARCH CARRIES NO FLOOR: it reads the lexical index, which each
// node's own walk maintains on its own schedule behind the applier, so a floor
// on the log would promise a visibility the index does not give.
var opWorkSearch = define("tracker.search", opRead, "", false,
	func(ctx context.Context, b Backend, _ *Actor, a workSearchArgs) (tracker.SearchAnswer, error) {
		if b.WorkSearch == nil {
			return tracker.SearchAnswer{}, errNoHalf
		}
		return b.WorkSearch.Search(ctx, tracker.SearchQuery{Text: a.Text, Limit: a.Limit, Mode: a.Mode})
	})

type workloadArgs struct {
	Query tracker.WorkloadQuery
	Now   time.Time

	// Zone is the company clock the asker cut its days on ([zoneArg]).
	Zone string
}

var opWorkload = define("tracker.workload", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a workloadArgs) (tracker.WorkloadAnswer, error) {
		if b.Tracker == nil {
			return tracker.WorkloadAnswer{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return tracker.WorkloadAnswer{}, err
		}
		a.Query.Units = b.Units
		return b.Tracker.Workload(ctx, a.Query, a.Now, loc)
	})

type inboxArgs struct {
	Query tracker.InboxQuery
	Now   time.Time
}

// opInbox is named `read_inbox`, beside `write_inbox`, never `tracker.inbox`:
// an operation's name is not a subject, but that one is spelled like a seat's
// inbox subject, and the guard that keeps every subject in internal/queue/topics
// rightly reads any literal ending `.inbox` as one written by hand.
var opInbox = define("tracker.read_inbox", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a inboxArgs) (tracker.InboxAnswer, error) {
		if b.Tracker == nil {
			return tracker.InboxAnswer{}, errNoHalf
		}
		return b.Tracker.Inbox(ctx, a.Query, a.Now)
	})

type routingArgs struct {
	Query tracker.RoutingQuery
	Now   time.Time
}

var opRouting = define("tracker.routing", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a routingArgs) (tracker.RoutingAnswer, error) {
		if b.Tracker == nil {
			return tracker.RoutingAnswer{}, errNoHalf
		}
		return b.Tracker.Routing(ctx, a.Query, a.Now)
	})

type turnsOfArgs struct {
	IDOrKey string
	Cursor  string
	Limit   int
	Fresh   statelog.Freshness
}

// A TASK'S TURNS are the tracker's: the turn records are charged to the task
// (ADR-0022) and live beside its rows.
var opTurnsOf = define("tracker.turns_of", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a turnsOfArgs) (tracker.TaskTurns, error) {
		if b.Tracker == nil {
			return tracker.TaskTurns{}, errNoHalf
		}
		return b.Tracker.TurnsOf(ctx, a.IDOrKey, a.Cursor, a.Limit, a.Fresh)
	})

// everyViewArgs is [viewsArgs] for every saved view.
type everyViewArgs struct {
	Query tracker.EveryViewQuery
	Zone  string
}

var opEveryView = define("tracker.every_view", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a everyViewArgs) (tracker.ViewListing, error) {
		if b.Tracker == nil {
			return tracker.ViewListing{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return tracker.ViewListing{}, err
		}
		q := a.Query
		q.Units, q.Zone = b.Units, loc
		return b.Tracker.EveryView(ctx, q)
	})

type flowArgs struct {
	Query tracker.FlowQuery
	Now   time.Time

	// Zone is the company clock the asker cut its days on ([zoneArg]).
	Zone string
}

var opFlow = define("tracker.flow", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a flowArgs) (tracker.FlowAnswer, error) {
		if b.Tracker == nil {
			return tracker.FlowAnswer{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return tracker.FlowAnswer{}, err
		}
		return b.Tracker.Flow(ctx, a.Query, a.Now, loc)
	})

var opCompanyFeed = define("tracker.company_feed", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q tracker.FeedQuery) (tracker.FeedPage, error) {
		if b.Tracker == nil {
			return tracker.FeedPage{}, errNoHalf
		}
		return b.Tracker.CompanyFeed(ctx, q)
	})

type decisionsArgs struct {
	Query tracker.DecisionsQuery
	Now   time.Time

	// Zone is the company clock the asker cut "overdue" on ([zoneArg]).
	Zone string
}

var opDecisions = define("tracker.decisions", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a decisionsArgs) (tracker.DecisionsAnswer, error) {
		if b.Tracker == nil {
			return tracker.DecisionsAnswer{}, errNoHalf
		}
		loc, err := zoneOf(a.Zone)
		if err != nil {
			return tracker.DecisionsAnswer{}, err
		}
		return b.Tracker.Decisions(ctx, a.Query, a.Now, loc)
	})

type turnPlacesArgs struct {
	Runs  []string
	Fresh statelog.Freshness
}

var opTurnPlaces = define("tracker.turn_places", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a turnPlacesArgs) (map[string]tracker.TurnPlace, error) {
		if b.Tracker == nil {
			return nil, errNoHalf
		}
		return b.Tracker.TurnPlaces(ctx, a.Runs, a.Fresh)
	})

// ---- the tracker's writes ------------------------------------------------ //

// A PROJECT'S FILES, as rows: the listing, one file with its manifest, and
// the two writes. The BYTES never cross here — a stateless node uploads and
// downloads chunks through its own object client, straight to the data nodes
// that hold them, and asks this service only for the row that names them.
var opFiles = define("tracker.files", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q tracker.FileQuery) (tracker.FileListing, error) {
		if b.Tracker == nil {
			return tracker.FileListing{}, errNoHalf
		}
		return b.Tracker.Files(ctx, q)
	})

type fileArgs struct {
	Project string
	Path    string
	Fresh   statelog.Freshness
}

var opFile = define("tracker.file", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a fileArgs) (tracker.FileDetail, error) {
		if b.Tracker == nil {
			return tracker.FileDetail{}, errNoHalf
		}
		return b.Tracker.File(ctx, a.Project, a.Path, a.Fresh)
	})

type putFileArgs struct {
	OpID string
	Put  tracker.FilePut
}

var opPutFile = define("tracker.put_file", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a putFileArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.PutFile(ctx, a.OpID, a.Put)
	})

type removeFileArgs struct {
	OpID    string
	Project string
	Path    string
	IfMatch uint64
}

var opRemoveFile = define("tracker.remove_file", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a removeFileArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.RemoveFile(ctx, a.OpID, a.Project, a.Path, a.IfMatch)
	})

type recordTurnArgs struct {
	OpID string
	Turn tracker.TurnRecord
}

// A TURN'S SPEND, recorded by the node that ran the turn — which on a node
// without `data` is a node with no applier, so it crosses like any other
// write. It carries its operation id, so an unanswered one is asked again of
// the next node: the applier adds a turn's spend only for a new operation, and
// a repeat under the same id counts nothing twice.
var opRecordTurn = define("tracker.record_turn", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a recordTurnArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.RecordTurn(ctx, a.OpID, a.Turn)
	})

// writerFor is the tracker writer acting as the request's actor.
func writerFor(b Backend, actor *Actor) (TrackerWriter, error) {
	if b.Writer == nil {
		return nil, errNoHalf
	}
	w := b.Writer(*actor)
	if w == nil {
		return nil, fmt.Errorf("estate: the tracker refused to act as %q (%s)",
			actor.Handle, actor.Kind)
	}
	return w, nil
}

type createTaskArgs struct {
	OpID   string
	Task   tracker.Task
	Notify *tracker.Notify
}

var opCreateTask = define("tracker.create_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a createTaskArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.CreateTask(ctx, a.OpID, a.Task, a.Notify)
	})

type createTaskAskingArgs struct {
	OpID   string
	Task   tracker.Task
	Ask    tracker.Comment
	Notify *tracker.Notify
}

var opCreateTaskAsking = define("tracker.create_task_asking", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a createTaskAskingArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.CreateTaskAsking(ctx, a.OpID, a.Task, a.Ask, a.Notify)
	})

type placeTaskArgs struct {
	OpID   string
	Place  tracker.Place
	Notify *tracker.Notify
}

// placedTask is [TrackerWriter.PlaceTask]'s answer as it crosses: the result,
// with the reason the card changed lanes and did not take its place carried
// BESIDE it as a wire error — an error is an interface no encoder writes, and
// the tool that reads it classifies the refusal by its identity (a stale
// version, a conflict), which [encodeError] keeps.
type placedTask struct {
	Result   tracker.PlaceResult
	Unplaced *wireError
}

// A DROP ON THE BOARD is a WALKING gesture on one task — its lane, then its
// place among its neighbours — each step under an operation id derived from
// the one the caller minted, so the whole of it is asked again of the next
// holder under the same id, and a step that holder's ledger answers is not
// written twice. A step one holder answered unvouched is not final
// ([unvouched]).
var opPlaceTask = define("tracker.place_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a placeTaskArgs) (placedTask, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return placedTask{}, err
		}
		res, err := w.PlaceTask(ctx, a.OpID, a.Place, a.Notify)
		out := placedTask{Unplaced: encodeError(res.Unplaced)}
		res.Unplaced = nil
		out.Result = res
		return out, err
	})

// updateTaskArgs carries a patch's GESTURES beside it: they are resolved
// inside the decide and never encoded into a record, which is why the patch
// itself does not serialise them — and why they travel here, named.
type updateTaskArgs struct {
	OpID      string
	ID        string
	Project   string
	IfMatch   uint64
	Patch     tracker.TaskPatch
	Watch     *tracker.WatchIntent
	Relate    *tracker.RelationIntent
	Depend    *tracker.DependentIntent
	Promote   *tracker.PromoteIntent
	Checklist *tracker.ChecklistIntent
	Kind      tracker.ChangeKind
	Notify    *tracker.Notify
}

var opUpdateTask = define("tracker.update_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a updateTaskArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		patch := a.Patch
		patch.Watch, patch.Relate, patch.Depend, patch.Promote, patch.Checklist =
			a.Watch, a.Relate, a.Depend, a.Promote, a.Checklist
		return w.UpdateTask(ctx, a.OpID, a.ID, a.Project, a.IfMatch, patch, a.Kind, a.Notify)
	})

type dependArgs struct {
	OpID   string
	Change tracker.DependencyChange
}

var opDepend = define("tracker.depend", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a dependArgs) (tracker.DependencyResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.DependencyResult{}, err
		}
		return w.Depend(ctx, a.OpID, a.Change, b.Leads)
	})

type mergeArgs struct {
	OpID      string
	Duplicate string
	Into      string
	Reparent  bool
	Notify    *tracker.Notify
}

var opMergeDuplicates = define("tracker.merge_duplicates", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a mergeArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.MergeDuplicates(ctx, a.OpID, a.Duplicate, a.Into, a.Reparent, a.Notify)
	})

type moveArgs struct {
	OpID   string
	TaskID string
	Target string
	Notify *tracker.Notify
}

var opMoveTaskToProject = define("tracker.move_task_to_project", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a moveArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.MoveTaskToProject(ctx, a.OpID, a.TaskID, a.Target, a.Notify)
	})

type writeProjectArgs struct {
	OpID      string
	Key       string
	Edit      tracker.ProjectEdit
	Authority tracker.ProjectAuthority
}

var opWriteProject = define("tracker.write_project", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeProjectArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WriteProject(ctx, a.OpID, a.Key, a.Edit, a.Authority)
	})

type writeTagsArgs struct {
	OpID      string
	Project   string
	Edit      tracker.TagEdit
	Authority tracker.TagAuthority
}

var opWriteTags = define("tracker.write_tags", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeTagsArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WriteTags(ctx, a.OpID, a.Project, a.Edit, a.Authority)
	})

type ensureTagsArgs struct {
	OpID    string
	Project string
	Tags    []string
}

// ensuredTags is [TrackerWriter.EnsureTags]' two answers, named.
type ensuredTags struct {
	Created  []string
	Warnings []string
}

var opEnsureTags = define("tracker.ensure_tags", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a ensureTagsArgs) (ensuredTags, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return ensuredTags{}, err
		}
		created, warnings, err := w.EnsureTags(ctx, a.OpID, a.Project, a.Tags)
		return ensuredTags{Created: created, Warnings: warnings}, err
	})

// ---- the operator's writes ------------------------------------------------ //

// The writes only the operator's surfaces make — no seat is handed a tool that
// reaches them — routed for the reason every other write is: the node the
// operator asked may hold no data, or a copy that is out of service.

type writeViewArgs struct {
	OpID string
	View tracker.View
}

var opWriteView = define("tracker.write_view", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeViewArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WriteView(ctx, a.OpID, a.View)
	})

type writeTypesArgs struct {
	OpID  string
	Types []tracker.TaskType
}

var opWriteTypes = define("tracker.write_types", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeTypesArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WriteTypes(ctx, a.OpID, a.Types)
	})

type writeFieldsArgs struct {
	OpID   string
	Fields []tracker.FieldDef
}

var opWriteFields = define("tracker.write_fields", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeFieldsArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WriteFields(ctx, a.OpID, a.Fields)
	})

type markInboxArgs struct {
	OpID    string
	Handle  string
	Gesture tracker.InboxGesture
}

// A GESTURE, never the lists: the tracker resolves it against the record its
// own decide reads, so a caller never sends back a list it read in another
// transaction — on whichever holder answers.
var opMarkInbox = define("tracker.mark_inbox", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a markInboxArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.MarkInbox(ctx, a.OpID, a.Handle, a.Gesture)
	})

type writePinsArgs struct {
	OpID    string
	Handle  string
	Gesture tracker.PinGesture
}

var opWritePins = define("tracker.write_pins", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writePinsArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WritePins(ctx, a.OpID, a.Handle, a.Gesture)
	})

type writePrioritiesArgs struct {
	OpID       string
	Handle     string
	Priorities []string
	IfMatch    *uint64
	Authority  tracker.PersonAuthority
}

var opWritePriorities = define("tracker.write_priorities", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writePrioritiesArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WritePriorities(ctx, a.OpID, a.Handle, a.Priorities, a.IfMatch, a.Authority)
	})

type removeTaskArgs struct {
	OpID    string
	ID      string
	Project string
	Subtree bool
	Notify  *tracker.Notify
}

var opRemoveTask = define("tracker.remove_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a removeTaskArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.RemoveTask(ctx, a.OpID, a.ID, a.Project, a.Subtree, a.Notify)
	})

type restoreTaskArgs struct {
	OpID    string
	ID      string
	Project string
	Notify  *tracker.Notify
}

var opRestoreTask = define("tracker.restore_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a restoreTaskArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.RestoreTask(ctx, a.OpID, a.ID, a.Project, a.Notify)
	})

type purgeTaskArgs struct {
	OpID    string
	ID      string
	Project string
	Reason  string
}

// A PURGE crosses like every other tracker write — its operation id is what a
// repeat after an unanswered one collapses on, and the purge's own ledger row
// answers that repeat with the first outcome — and its refusals keep their
// identity, so an operator re-running one is told it is purged, not behind.
var opPurgeTask = define("tracker.purge_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a purgeTaskArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.PurgeTask(ctx, a.OpID, a.ID, a.Project, a.Reason)
	})

// ---- the knowledge base -------------------------------------------------- //

type listPagesArgs struct {
	Filter pages.Filter
	Fresh  statelog.Freshness
}

var opListPages = define("pages.list", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a listPagesArgs) (pages.Listing, error) {
		if b.Pages == nil {
			return pages.Listing{}, errNoHalf
		}
		return b.Pages.List(ctx, a.Filter, a.Fresh)
	})

type getPageArgs struct {
	Ref   string
	Fresh statelog.Freshness
}

var opGetPage = define("pages.get", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a getPageArgs) (pages.Detail, error) {
		if b.Pages == nil {
			return pages.Detail{}, errNoHalf
		}
		return b.Pages.Get(ctx, a.Ref, a.Fresh)
	})

type skillPagesArgs struct {
	Container string
	Fresh     statelog.Freshness
}

var opSkillPages = define("pages.skill_pages", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a skillPagesArgs) ([]pages.Page, error) {
		if b.Pages == nil {
			return nil, errNoHalf
		}
		return b.Pages.SkillPages(ctx, a.Container, a.Fresh)
	})

type containersArgs struct {
	Fresh statelog.Freshness
}

var opContainers = define("pages.containers", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a containersArgs) ([]pages.ContainerListing, error) {
		if b.Pages == nil {
			return nil, errNoHalf
		}
		return b.Pages.Containers(ctx, a.Fresh)
	})

// A PAGE'S ACTIVITY, a container's, or the whole knowledge base's.
var opPageActivity = define("pages.activity", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q pages.PageActivityQuery) (pages.PageActivity, error) {
		if b.Pages == nil {
			return pages.PageActivity{}, errNoHalf
		}
		return b.Pages.Activity(ctx, q)
	})

type revisionArgs struct {
	PageID  string
	Version int
	Fresh   statelog.Freshness
}

// aRevision is [PageReader.Revision]'s two answers, named.
type aRevision struct {
	Revision pages.Revision
	Found    bool
}

var opRevision = define("pages.revision", opRead, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a revisionArgs) (aRevision, error) {
		if b.Pages == nil {
			return aRevision{}, errNoHalf
		}
		rev, found, err := b.Pages.Revision(ctx, a.PageID, a.Version, a.Fresh)
		return aRevision{Revision: rev, Found: found}, err
	})

type createPageArgs struct {
	Actor pages.Actor
	Page  pages.NewPage
}

var opCreatePage = define("pages.create", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a createPageArgs) (pages.Written, error) {
		if b.PageWriter == nil {
			return pages.Written{}, errNoHalf
		}
		return b.PageWriter.Create(ctx, a.Actor, a.Page)
	}).repeatableWhen(func(a createPageArgs) bool { return a.Page.CallKey.String() != "" })

type savePageArgs struct {
	Actor  pages.Actor
	PageID string
	Save   pages.Save
}

var opSavePage = define("pages.save", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a savePageArgs) (pages.Written, error) {
		if b.PageWriter == nil {
			return pages.Written{}, errNoHalf
		}
		return b.PageWriter.SavePage(ctx, a.Actor, a.PageID, a.Save)
	}).repeatableWhen(func(a savePageArgs) bool { return a.Save.CallKey.String() != "" })

type renamePageArgs struct {
	Actor  pages.Actor
	PageID string
	Title  string
	Quiet  bool
	Key    pages.CallKey
}

var opRenamePage = define("pages.rename", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a renamePageArgs) (pages.Written, error) {
		if b.PageWriter == nil {
			return pages.Written{}, errNoHalf
		}
		return b.PageWriter.Rename(ctx, a.Actor, a.PageID, a.Title, a.Quiet, a.Key)
	}).repeatableWhen(func(a renamePageArgs) bool { return a.Key.String() != "" })

type commentArgs struct {
	Actor   pages.Actor
	PageID  string
	Comment pages.NewComment
}

// commented is a comment write's two answers, named.
type commented struct {
	Comment pages.Comment
	Written pages.Written
}

var opCommentPage = define("pages.comment", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a commentArgs) (commented, error) {
		if b.PageWriter == nil {
			return commented{}, errNoHalf
		}
		c, w, err := b.PageWriter.Comment(ctx, a.Actor, a.PageID, a.Comment)
		return commented{Comment: c, Written: w}, err
	}).repeatableWhen(func(a commentArgs) bool { return a.Comment.CallKey.String() != "" })

type editCommentArgs struct {
	Actor     pages.Actor
	PageID    string
	CommentID string
	Body      string
	Key       pages.CallKey
}

var opEditComment = define("pages.edit_comment", opOnceWrite, pagesDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a editCommentArgs) (commented, error) {
		if b.PageWriter == nil {
			return commented{}, errNoHalf
		}
		c, w, err := b.PageWriter.EditComment(ctx, a.Actor, a.PageID, a.CommentID, a.Body, a.Key)
		return commented{Comment: c, Written: w}, err
	}).repeatableWhen(func(a editCommentArgs) bool { return a.Key.String() != "" })

// knowledgeArgs is a search as it crosses. The seat is its HANDLE and the
// org is the serving node's own, for the reason the package doc gives:
// neither is a value, both are the current chart.
type knowledgeArgs struct {
	Text  string
	Seat  string
	Limit int

	// Mode is how to rank. EMPTY FROM AN OLDER BUILD, which ranked hybrid
	// and is answered as it always was: the zero value is hybrid.
	Mode knowledge.Mode

	// Scoped says the caller searched with an org, which is what supplies
	// the read scope; false searches as nobody.
	Scoped bool

	// ExcludeAncestors keeps nil and empty APART, which the query tells
	// apart on purpose: nil takes the default exclusion, and an empty list
	// is a caller deliberately searching drafts. Omitted-if-empty would
	// collapse the second into the first.
	ExcludeAncestors []string
	Exclusion        bool
}

// query is the search a request names, against the serving node's own chart:
// the seat's role and the org whose read scope applies.
func (a knowledgeArgs) query(b Backend) knowledge.Query {
	q := knowledge.Query{Text: a.Text, Limit: a.Limit, Mode: a.Mode}
	if a.Exclusion {
		q.ExcludeAncestors = a.ExcludeAncestors
		if q.ExcludeAncestors == nil {
			q.ExcludeAncestors = []string{}
		}
	}
	if b.Seat != nil {
		seat, o := b.Seat(a.Seat)
		if a.Seat != "" {
			q.Seat = seat
		}
		if a.Scoped {
			q.Org = o
		}
	}
	return q
}

// THE KNOWLEDGE SEARCH CARRIES NO FLOOR, for [opWorkSearch]'s reason: it
// reads the index, which no log position describes.
var opKnowledgeSearch = define("knowledge.search", opRead, "", false,
	func(ctx context.Context, b Backend, _ *Actor, a knowledgeArgs) (knowledge.Result, error) {
		if b.Knowledge == nil {
			return knowledge.Result{}, errNoHalf
		}
		return b.Knowledge.Answer(ctx, a.query(b))
	})

// WHETHER THE INDEX IS STILL BUILDING, which is what turns an empty search into
// "still indexing" — asked, like the search, of the index rather than the log.
var opKnowledgeBuilding = define("knowledge.building", opRead, "", false,
	func(ctx context.Context, b Backend, _ *Actor, _ struct{}) (bool, error) {
		if b.Knowledge == nil {
			return false, errNoHalf
		}
		return b.Knowledge.Building(ctx), nil
	})

// served is what a node runs, as a seat's admission asks it.
type served struct {
	Tracker bool
	Pages   bool
}

// opPing is ADMISSION's question, asked of a data node: whether its copy
// admits a seat now ([Backend.Admits]), and which halves it runs natively. A
// copy that does not is passed over, like a node that ran nothing — see
// [Router.Serves].
//
// NO FLOOR, and past the serving gate on purpose: it asks whether the copy is
// ready for a seat, which is its own stricter gate, not whether it has
// reached this node's writes. And NO ARGUMENTS: the one estate is the only
// thing it can be about.
var opPing = define("estate.ping", opRead, "", false,
	func(ctx context.Context, b Backend, _ *Actor, _ struct{}) (served, error) {
		if b.Admits != nil && !b.Admits(ctx) {
			return served{}, errNotAdmitting
		}
		return served{Tracker: b.Tracker != nil, Pages: b.Pages != nil}, nil
	}).ungated()
