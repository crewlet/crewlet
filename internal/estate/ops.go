package estate

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
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
	MyWork(ctx context.Context, q tracker.MyWorkQuery, now time.Time) (tracker.MyWork, error)
	Projects(ctx context.Context, q tracker.ProjectQuery) (tracker.ProjectListing, error)
	Project(ctx context.Context, q tracker.ProjectDetailQuery) (tracker.ProjectDetail, error)
	Files(ctx context.Context, q tracker.FileQuery) (tracker.FileListing, error)
	File(ctx context.Context, project, path string, fresh statelog.Freshness) (tracker.FileDetail, error)

	// The operator's reads: a unit's workload, a person's inbox and the
	// routing a change took.
	Workload(ctx context.Context, q tracker.WorkloadQuery, now time.Time) (tracker.WorkloadAnswer, error)
	Inbox(ctx context.Context, q tracker.InboxQuery, now time.Time) (tracker.InboxAnswer, error)
	Routing(ctx context.Context, q tracker.RoutingQuery, now time.Time) (tracker.RoutingAnswer, error)
}

// TrackerWriter is the tracker's write side, as ONE actor — see [Actor].
type TrackerWriter interface {
	CreateTask(ctx context.Context, opID string, task tracker.Task,
		notify *tracker.Notify) (tracker.WriteResult, error)
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
	WriteInbox(ctx context.Context, opID, handle string, read, unread, snoozed []tracker.InboxEntry,
		reasons []tracker.Reason, seenThrough tracker.Position) (tracker.WriteResult, error)
	WritePins(ctx context.Context, opID, handle string, pinnedViews []string,
		favorites []tracker.Favorite) (tracker.WriteResult, error)
	WritePriorities(ctx context.Context, opID, handle string, priorities []string,
		authority tracker.PersonAuthority) (tracker.WriteResult, error)
	RemoveTask(ctx context.Context, opID, id, project string, subtree bool,
		notify *tracker.Notify) (tracker.WriteResult, error)
	RestoreTask(ctx context.Context, opID, id, project string,
		notify *tracker.Notify) (tracker.WriteResult, error)
	PurgeTask(ctx context.Context, opID, id, project, reason string) (tracker.WriteResult, error)
}

// WorkSearcher is the tracker's ranked search over one partition's corpus,
// before the fusion a gather makes across partitions ([tracker.MergeSearch]).
type WorkSearcher interface {
	Slice(ctx context.Context, text string) (tracker.SearchSlice, error)
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
	Rename(ctx context.Context, actor pages.Actor, pageID, title string, quiet bool) (pages.Written, error)
	Comment(ctx context.Context, actor pages.Actor, pageID string,
		in pages.NewComment) (pages.Comment, pages.Written, error)
	EditComment(ctx context.Context, actor pages.Actor, pageID, commentID,
		body string) (pages.Comment, pages.Written, error)
}

// KnowledgeSearcher is the native knowledge search over one partition's
// corpus, before the fusion a gather makes across partitions
// ([pages.MergeSearch]), with the one question a caller asks of an empty
// answer.
type KnowledgeSearcher interface {
	Slice(ctx context.Context, q knowledge.Query) (pages.SearchSlice, error)
	Building(ctx context.Context) bool
}

// Backend is one serving node's answer to every operation on ONE partition it
// serves, resolved PER REQUEST: a node that booted with no company brings its
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

	// Committed waits until this node's applier of the partition holds a
	// position — the floor every request carries, the node's own included.
	Committed func(ctx context.Context, at statelog.Position) error

	// Answers is whether this copy of the partition may answer a request
	// now ([statelog.Health.Answers]): serving — drained since its appliers
	// started and within the snapshot slack of every log's end — or level
	// this instant. A copy that does not LAGS: it is asked only once every
	// holder whose copy does not has run nothing, by a remote asker and by
	// this node's own router alike — a worse holder, never no holder
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

	// Applied is where this copy's applier of one of the partition's logs
	// has committed, read BEFORE a read that reports its coverage begins —
	// so the cut it reports is a lower bound on what the answer holds
	// ([statelog.Coverage.At]). Nil states no cut.
	Applied func(stream string) statelog.Position

	// Barrier appends a barrier on one of the partition's logs and waits
	// for this copy to apply through it ([statelog.Reader.Barrier]) — the
	// `linearizable` half of a gather slice. False where the log keeps no
	// read index: it makes no freshness claim (the vectors' derived log),
	// and where it was is what [Backend.Applied] says.
	Barrier func(ctx context.Context, stream string) (statelog.Position, bool, error)

	// Gates is this copy's writer of a node gate's record on one of the
	// partition's identity-claiming logs, by the log's domain: the
	// `statelog.gate` operation's server half, publishing through this
	// node's own write authority on that log on behalf of the node an
	// operator asked ([OpStatelogGate]). Nil where this node writes no gate
	// record at all right now, and a nil writer for a domain is a log of
	// the partition it does not run right now — each answered "no native
	// backend here", so the request moves on to the next holder.
	Gates func(domain string) GateWriter

	// ServerSeams are the node's own, partition-free: the dispatcher sets
	// them on every backend it hands an operation.
	ServerSeams
}

// ServerSeams are what an operation reads of the SERVING node rather than of a
// partition: the chart seams a query or a dependency consults and the org a
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

	// Events is this node's own event log, which takes custody of the
	// records a node holding no store publishes. See [opAppendEvents].
	Events EventSink
}

// EventSink is somewhere an event record is persisted, idempotently on its
// identity.
type EventSink interface {
	Append(ctx context.Context, rec store.EventRecord) error
}

// errNoHalf is an operation on a half this node does not run.
var errNoHalf = errors.New("estate: this node runs no native backend for this operation")

// errNotAdmitting is admission's ping on a copy that admits no seat yet
// ([Backend.Admits]): nothing ran, and the asker moves on.
var errNotAdmitting = errors.New("estate: this copy admits no seat yet")

// The domains an operation addresses as one partition, and how the ones that
// name an object find it.
var (
	trackerDomain = tracker.Domain{}.Name()
	pagesDomain   = pages.Domain{}.Name()
	vectorsDomain = search.Domain{}.Name()
)

// ---- the tracker's reads ------------------------------------------------ //

type tasksArgs struct {
	Query tracker.Query
	Now   time.Time
}

var opTasks = define("tracker.tasks", opRead, wholeDomain[tasksArgs](trackerDomain), false,
	func(ctx context.Context, b Backend, _ *Actor, a tasksArgs) (tracker.Answer, error) {
		if b.Tracker == nil {
			return tracker.Answer{}, errNoHalf
		}
		a.Query.Units = b.Units
		return b.Tracker.Tasks(ctx, a.Query, a.Now)
	}).covered(
	func(a *tracker.Answer, c statelog.Coverage) { a.Coverage = c })

type taskArgs struct {
	IDOrKey string
	Want    tracker.DetailWants
	Fresh   statelog.Freshness
}

var opTask = define("tracker.task", opRead, byTask(func(a taskArgs) string { return a.IDOrKey }), false,
	func(ctx context.Context, b Backend, _ *Actor, a taskArgs) (tracker.TaskDetail, error) {
		if b.Tracker == nil {
			return tracker.TaskDetail{}, errNoHalf
		}
		a.Want.Units = b.Units
		return b.Tracker.Task(ctx, a.IDOrKey, a.Want, a.Fresh)
	})

var opViews = define("tracker.views", opRead, wholeDomain[tracker.ViewQuery](trackerDomain), false,
	func(ctx context.Context, b Backend, _ *Actor, q tracker.ViewQuery) (tracker.ViewListing, error) {
		if b.Tracker == nil {
			return tracker.ViewListing{}, errNoHalf
		}
		q.Units = b.Units
		return b.Tracker.Views(ctx, q)
	})

type expandArgs struct {
	Params map[string]any
	Viewer tracker.Viewer
	Now    time.Time

	// Zone is the location's NAME, which is what crosses: a
	// *time.Location is a table of transitions a JSON encoder writes as
	// nothing at all. Empty is a nil location.
	Zone string
}

var opExpandedQuery = define("tracker.expanded_query", opRead, wholeDomain[expandArgs](trackerDomain), false,
	func(ctx context.Context, b Backend, _ *Actor, a expandArgs) (tracker.Query, error) {
		if b.Tracker == nil {
			return tracker.Query{}, errNoHalf
		}
		var loc *time.Location
		if a.Zone != "" {
			var err error
			if loc, err = time.LoadLocation(a.Zone); err != nil {
				return tracker.Query{}, fmt.Errorf("estate: the asking node's "+
					"zone %q is not one this node can load: %w", a.Zone, err)
			}
		}
		return b.Tracker.ExpandedQuery(ctx, a.Params, a.Viewer, a.Now, loc)
	})

var opCatalogue = define("tracker.catalogue", opRead, wholeDomain[tracker.CatalogueQuery](trackerDomain), false,
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

var opPerson = define("tracker.person", opRead, wholeDomain[personArgs](trackerDomain), false,
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

var opThread = define("tracker.thread", opRead, byTask(func(a threadArgs) string { return a.Query.Task }), false,
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

var opActivity = define("tracker.activity", opRead, wholeDomain[activityArgs](trackerDomain), false,
	func(ctx context.Context, b Backend, _ *Actor, a activityArgs) (tracker.ActivityAnswer, error) {
		if b.Tracker == nil {
			return tracker.ActivityAnswer{}, errNoHalf
		}
		return b.Tracker.Activity(ctx, a.Query, a.Now)
	}).covered(
	func(a *tracker.ActivityAnswer, c statelog.Coverage) { a.Coverage = c })

type myWorkArgs struct {
	Query tracker.MyWorkQuery
	Now   time.Time
}

var opMyWork = define("tracker.my_work", opRead, wholeDomain[myWorkArgs](trackerDomain), false,
	func(ctx context.Context, b Backend, _ *Actor, a myWorkArgs) (tracker.MyWork, error) {
		if b.Tracker == nil {
			return tracker.MyWork{}, errNoHalf
		}
		return b.Tracker.MyWork(ctx, a.Query, a.Now)
	}).covered(
	func(w *tracker.MyWork, c statelog.Coverage) { w.Coverage = c })

var opProjects = define("tracker.projects", opRead, wholeDomain[tracker.ProjectQuery](trackerDomain), false,
	func(ctx context.Context, b Backend, _ *Actor, q tracker.ProjectQuery) (tracker.ProjectListing, error) {
		if b.Tracker == nil {
			return tracker.ProjectListing{}, errNoHalf
		}
		q.Units = b.Units
		return b.Tracker.Projects(ctx, q)
	}).covered(
	func(l *tracker.ProjectListing, c statelog.Coverage) { l.Coverage = c })

var opProject = define("tracker.project", opRead, wholeDomain[tracker.ProjectDetailQuery](trackerDomain), false,
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
}

// THE RANKED SEARCH IS A GATHER over every partition holding the corpus of
// work items: each answers its candidates, and the fusion is made where they
// are all held ([tracker.MergeSearch]) — the arithmetic one corpus is ranked
// by.
//
// NO FLOOR: the ranked search reads the lexical index, which each node's own
// walk maintains on its own schedule behind the applier, so a floor on the log
// would promise a visibility the index does not give.
var opWorkSearch = defineGather("tracker.search", corpusOf[workSearchArgs](trackerDomain),
	func(ctx context.Context, b Backend, _ statelog.PartitionID, a workSearchArgs) (tracker.SearchSlice, error) {
		if b.WorkSearch == nil {
			return tracker.SearchSlice{}, errNoHalf
		}
		return b.WorkSearch.Slice(ctx, a.Text)
	},
	func(a workSearchArgs, g Gathered[tracker.SearchSlice]) ([]tracker.Ranked, error) {
		return tracker.MergeSearch(g.Answered(), a.Limit), nil
	}).floorless()

type workloadArgs struct {
	Query tracker.WorkloadQuery
	Now   time.Time
}

var opWorkload = define("tracker.workload", opRead, wholeDomain[workloadArgs](trackerDomain), false,
	func(ctx context.Context, b Backend, _ *Actor, a workloadArgs) (tracker.WorkloadAnswer, error) {
		if b.Tracker == nil {
			return tracker.WorkloadAnswer{}, errNoHalf
		}
		a.Query.Units = b.Units
		return b.Tracker.Workload(ctx, a.Query, a.Now)
	})

type inboxArgs struct {
	Query tracker.InboxQuery
	Now   time.Time
}

// opInbox is named `read_inbox`, beside `write_inbox`, never `tracker.inbox`:
// an operation's name is not a subject, but that one is spelled like a seat's
// inbox subject, and the guard that keeps every subject in internal/queue/topics
// rightly reads any literal ending `.inbox` as one written by hand.
var opInbox = define("tracker.read_inbox", opRead, wholeDomain[inboxArgs](trackerDomain), false,
	func(ctx context.Context, b Backend, _ *Actor, a inboxArgs) (tracker.InboxAnswer, error) {
		if b.Tracker == nil {
			return tracker.InboxAnswer{}, errNoHalf
		}
		return b.Tracker.Inbox(ctx, a.Query, a.Now)
	}).covered(
	func(a *tracker.InboxAnswer, c statelog.Coverage) { a.Coverage = c })

type routingArgs struct {
	Query tracker.RoutingQuery
	Now   time.Time
}

var opRouting = define("tracker.routing", opRead, wholeDomain[routingArgs](trackerDomain), false,
	func(ctx context.Context, b Backend, _ *Actor, a routingArgs) (tracker.RoutingAnswer, error) {
		if b.Tracker == nil {
			return tracker.RoutingAnswer{}, errNoHalf
		}
		return b.Tracker.Routing(ctx, a.Query, a.Now)
	})

// ---- the tracker's writes ------------------------------------------------ //

// A PROJECT'S FILES, as rows: the listing, one file with its manifest, and
// the two writes. The BYTES never cross here — a stateless node uploads and
// downloads chunks through its own object client, straight to the data nodes
// that hold them, and asks this service only for the row that names them.
var opFiles = define("tracker.files", opRead, wholeDomain[tracker.FileQuery](trackerDomain), false,
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

var opFile = define("tracker.file", opRead, wholeDomain[fileArgs](trackerDomain), false,
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

var opPutFile = define("tracker.put_file", opIdempotentWrite, wholeDomain[putFileArgs](trackerDomain), true,
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

var opRemoveFile = define("tracker.remove_file", opIdempotentWrite, wholeDomain[removeFileArgs](trackerDomain), true,
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
var opRecordTurn = define("tracker.record_turn", opIdempotentWrite, byTask(func(a recordTurnArgs) string { return a.Turn.Task }), true,
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

var opCreateTask = define("tracker.create_task", opIdempotentWrite, wholeDomain[createTaskArgs](trackerDomain), true,
	func(ctx context.Context, b Backend, actor *Actor, a createTaskArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.CreateTask(ctx, a.OpID, a.Task, a.Notify)
	})

// updateTaskArgs carries a patch's GESTURES beside it: they are resolved
// inside the decide and never encoded into a record, which is why the patch
// itself does not serialise them — and why they travel here, named.
type updateTaskArgs struct {
	OpID    string
	ID      string
	Project string
	IfMatch uint64
	Patch   tracker.TaskPatch
	Watch   *tracker.WatchIntent
	Relate  *tracker.RelationIntent
	Depend  *tracker.DependentIntent
	Promote *tracker.PromoteIntent
	Kind    tracker.ChangeKind
	Notify  *tracker.Notify
}

var opUpdateTask = define("tracker.update_task", opIdempotentWrite, byTask(func(a updateTaskArgs) string { return a.ID }), true,
	func(ctx context.Context, b Backend, actor *Actor, a updateTaskArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		patch := a.Patch
		patch.Watch, patch.Relate, patch.Depend, patch.Promote = a.Watch, a.Relate, a.Depend, a.Promote
		return w.UpdateTask(ctx, a.OpID, a.ID, a.Project, a.IfMatch, patch, a.Kind, a.Notify)
	})

type dependArgs struct {
	OpID   string
	Change tracker.DependencyChange
}

var opDepend = define("tracker.depend", opIdempotentWrite, byTask(func(a dependArgs) string { return a.Change.Task }), true,
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

var opMergeDuplicates = define("tracker.merge_duplicates", opIdempotentWrite, byTask(func(a mergeArgs) string { return a.Duplicate }), true,
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

var opMoveTaskToProject = define("tracker.move_task_to_project", opIdempotentWrite, byTask(func(a moveArgs) string { return a.TaskID }), true,
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

var opWriteProject = define("tracker.write_project", opIdempotentWrite, wholeDomain[writeProjectArgs](trackerDomain), true,
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

var opWriteTags = define("tracker.write_tags", opIdempotentWrite, wholeDomain[writeTagsArgs](trackerDomain), true,
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

var opEnsureTags = define("tracker.ensure_tags", opIdempotentWrite, wholeDomain[ensureTagsArgs](trackerDomain), true,
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

var opWriteView = define("tracker.write_view", opIdempotentWrite, wholeDomain[writeViewArgs](trackerDomain), true,
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

var opWriteTypes = define("tracker.write_types", opIdempotentWrite, wholeDomain[writeTypesArgs](trackerDomain), true,
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

var opWriteFields = define("tracker.write_fields", opIdempotentWrite, wholeDomain[writeFieldsArgs](trackerDomain), true,
	func(ctx context.Context, b Backend, actor *Actor, a writeFieldsArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WriteFields(ctx, a.OpID, a.Fields)
	})

type writeInboxArgs struct {
	OpID        string
	Handle      string
	Read        []tracker.InboxEntry
	Unread      []tracker.InboxEntry
	Snoozed     []tracker.InboxEntry
	Reasons     []tracker.Reason
	SeenThrough tracker.Position
}

var opWriteInbox = define("tracker.write_inbox", opIdempotentWrite, wholeDomain[writeInboxArgs](trackerDomain), true,
	func(ctx context.Context, b Backend, actor *Actor, a writeInboxArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WriteInbox(ctx, a.OpID, a.Handle, a.Read, a.Unread, a.Snoozed, a.Reasons, a.SeenThrough)
	})

type writePinsArgs struct {
	OpID        string
	Handle      string
	PinnedViews []string
	Favorites   []tracker.Favorite
}

var opWritePins = define("tracker.write_pins", opIdempotentWrite, wholeDomain[writePinsArgs](trackerDomain), true,
	func(ctx context.Context, b Backend, actor *Actor, a writePinsArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WritePins(ctx, a.OpID, a.Handle, a.PinnedViews, a.Favorites)
	})

type writePrioritiesArgs struct {
	OpID       string
	Handle     string
	Priorities []string
	Authority  tracker.PersonAuthority
}

var opWritePriorities = define("tracker.write_priorities", opIdempotentWrite, wholeDomain[writePrioritiesArgs](trackerDomain), true,
	func(ctx context.Context, b Backend, actor *Actor, a writePrioritiesArgs) (tracker.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return tracker.WriteResult{}, err
		}
		return w.WritePriorities(ctx, a.OpID, a.Handle, a.Priorities, a.Authority)
	})

type removeTaskArgs struct {
	OpID    string
	ID      string
	Project string
	Subtree bool
	Notify  *tracker.Notify
}

var opRemoveTask = define("tracker.remove_task", opIdempotentWrite, byTask(func(a removeTaskArgs) string { return a.ID }), true,
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

var opRestoreTask = define("tracker.restore_task", opIdempotentWrite, byTask(func(a restoreTaskArgs) string { return a.ID }), true,
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
var opPurgeTask = define("tracker.purge_task", opIdempotentWrite, byTask(func(a purgeTaskArgs) string { return a.ID }), true,
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

var opListPages = define("pages.list", opRead, wholeDomain[listPagesArgs](pagesDomain), false,
	func(ctx context.Context, b Backend, _ *Actor, a listPagesArgs) (pages.Listing, error) {
		if b.Pages == nil {
			return pages.Listing{}, errNoHalf
		}
		return b.Pages.List(ctx, a.Filter, a.Fresh)
	}).covered(
	func(l *pages.Listing, c statelog.Coverage) { l.Coverage = c })

type getPageArgs struct {
	Ref   string
	Fresh statelog.Freshness
}

var opGetPage = define("pages.get", opRead, byPage(func(a getPageArgs) string { return a.Ref }), false,
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

var opSkillPages = define("pages.skill_pages", opRead, wholeDomain[skillPagesArgs](pagesDomain), false,
	func(ctx context.Context, b Backend, _ *Actor, a skillPagesArgs) ([]pages.Page, error) {
		if b.Pages == nil {
			return nil, errNoHalf
		}
		return b.Pages.SkillPages(ctx, a.Container, a.Fresh)
	})

type containersArgs struct {
	Fresh statelog.Freshness
}

var opContainers = define("pages.containers", opRead, wholeDomain[containersArgs](pagesDomain), false,
	func(ctx context.Context, b Backend, _ *Actor, a containersArgs) ([]pages.ContainerListing, error) {
		if b.Pages == nil {
			return nil, errNoHalf
		}
		return b.Pages.Containers(ctx, a.Fresh)
	})

// A PAGE'S ACTIVITY is that page's partition's; a container's, or the whole
// knowledge base's, addresses the domain as one.
var opPageActivity = define("pages.activity", opRead,
	address[pages.PageActivityQuery]{domain: pagesDomain, partitions: func(ctx context.Context,
		l statelog.Layout, r Resolver, q pages.PageActivityQuery) ([]statelog.PartitionID, error) {
		if q.Page != "" {
			return byPage(func(q pages.PageActivityQuery) string { return q.Page }).partitions(ctx, l, r, q)
		}
		return wholeDomain[pages.PageActivityQuery](pagesDomain).partitions(ctx, l, r, q)
	}}, false,
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

var opRevision = define("pages.revision", opRead, byPage(func(a revisionArgs) string { return a.PageID }), false,
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

var opCreatePage = define("pages.create", opOnceWrite, wholeDomain[createPageArgs](pagesDomain), false,
	func(ctx context.Context, b Backend, _ *Actor, a createPageArgs) (pages.Written, error) {
		if b.PageWriter == nil {
			return pages.Written{}, errNoHalf
		}
		return b.PageWriter.Create(ctx, a.Actor, a.Page)
	})

type savePageArgs struct {
	Actor  pages.Actor
	PageID string
	Save   pages.Save
}

var opSavePage = define("pages.save", opOnceWrite, byPage(func(a savePageArgs) string { return a.PageID }), false,
	func(ctx context.Context, b Backend, _ *Actor, a savePageArgs) (pages.Written, error) {
		if b.PageWriter == nil {
			return pages.Written{}, errNoHalf
		}
		return b.PageWriter.SavePage(ctx, a.Actor, a.PageID, a.Save)
	})

type renamePageArgs struct {
	Actor  pages.Actor
	PageID string
	Title  string
	Quiet  bool
}

var opRenamePage = define("pages.rename", opOnceWrite, byPage(func(a renamePageArgs) string { return a.PageID }), false,
	func(ctx context.Context, b Backend, _ *Actor, a renamePageArgs) (pages.Written, error) {
		if b.PageWriter == nil {
			return pages.Written{}, errNoHalf
		}
		return b.PageWriter.Rename(ctx, a.Actor, a.PageID, a.Title, a.Quiet)
	})

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

var opCommentPage = define("pages.comment", opOnceWrite, byPage(func(a commentArgs) string { return a.PageID }), false,
	func(ctx context.Context, b Backend, _ *Actor, a commentArgs) (commented, error) {
		if b.PageWriter == nil {
			return commented{}, errNoHalf
		}
		c, w, err := b.PageWriter.Comment(ctx, a.Actor, a.PageID, a.Comment)
		return commented{Comment: c, Written: w}, err
	})

type editCommentArgs struct {
	Actor     pages.Actor
	PageID    string
	CommentID string
	Body      string
}

var opEditComment = define("pages.edit_comment", opOnceWrite, byPage(func(a editCommentArgs) string { return a.PageID }), false,
	func(ctx context.Context, b Backend, _ *Actor, a editCommentArgs) (commented, error) {
		if b.PageWriter == nil {
			return commented{}, errNoHalf
		}
		c, w, err := b.PageWriter.EditComment(ctx, a.Actor, a.PageID, a.CommentID, a.Body)
		return commented{Comment: c, Written: w}, err
	})

// knowledgeArgs is a search as it crosses. The seat is its HANDLE and the
// org is the serving node's own, for the reason the package doc gives:
// neither is a value, both are the current chart.
type knowledgeArgs struct {
	Text  string
	Seat  string
	Limit int

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
	q := knowledge.Query{Text: a.Text, Limit: a.Limit}
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

// THE KNOWLEDGE SEARCH IS A GATHER over every partition holding the knowledge
// base's corpus, on [opWorkSearch]'s terms: each partition answers its
// candidates and the pages behind them, and the fusion is made where they are
// all held ([pages.MergeSearch]).
var opKnowledgeSearch = defineGather("knowledge.search", corpusOf[knowledgeArgs](pagesDomain),
	func(ctx context.Context, b Backend, _ statelog.PartitionID, a knowledgeArgs) (pages.SearchSlice, error) {
		if b.Knowledge == nil {
			return pages.SearchSlice{}, errNoHalf
		}
		return b.Knowledge.Slice(ctx, a.query(b))
	},
	func(a knowledgeArgs, g Gathered[pages.SearchSlice]) ([]knowledge.Hit, error) {
		return pages.MergeSearch(g.Answered(), knowledge.Query{Limit: a.Limit}), nil
	}).floorless()

// WHETHER AN INDEX IS STILL BUILDING is asked of every partition a search
// reads: an empty answer is "still indexing" if ANY of them is, since that
// partition's documents are the ones the answer could not have found.
var opKnowledgeBuilding = defineGather("knowledge.building", corpusOf[struct{}](pagesDomain),
	func(ctx context.Context, b Backend, _ statelog.PartitionID, _ struct{}) (bool, error) {
		if b.Knowledge == nil {
			return false, errNoHalf
		}
		return b.Knowledge.Building(ctx), nil
	},
	func(_ struct{}, g Gathered[bool]) (bool, error) {
		return slices.Contains(g.Answered(), true), nil
	}).floorless()

// served is what a node runs, as a seat's admission asks it.
type served struct {
	Tracker bool
	Pages   bool
}

// pingArgs names the partition admission asks about.
//
// EMPTY FROM AN OLDER BUILD, whose admission asked this question of the whole
// estate with no arguments at all — and a rolling upgrade puts that build's
// stateless nodes in front of this build's data nodes. So a ping naming no
// partition is that older question and is answered as it was: of the one
// partition of a layout that has one, which is every layout an older build
// can share a fleet with (a layout that divides the estate is fenced by the
// protocol version such a build cannot pass), and [ErrUnaddressed] under any
// other — the answer every whole-estate operation gives there.
type pingArgs struct {
	Partition string
}

// pingPartitions is the partition admission asks about: the one it names, or
// the whole estate's one partition for an older asker, which names none.
func pingPartitions(_ context.Context, l statelog.Layout, _ Resolver, a pingArgs) (
	[]statelog.PartitionID, error) {

	if a.Partition == "" {
		if parts := l.Partitions(); len(parts) == 1 {
			return parts, nil
		}
		return nil, fmt.Errorf("%w: an admission question that names no partition "+
			"asks about the whole estate, and layout %d divides it into %d partitions",
			ErrUnaddressed, l.Number, len(l.Partitions()))
	}
	p, err := statelog.ParsePartitionID(a.Partition)
	if err != nil {
		return nil, fmt.Errorf("admission names %q: %w", a.Partition, err)
	}
	if len(l.Logs(p)) == 0 {
		return nil, fmt.Errorf("%w: layout %d has no partition %s", ErrUnaddressed, l.Number, p)
	}
	return []statelog.PartitionID{p}, nil
}

// opPing is ADMISSION's question, asked of a holder of the partition a seat
// needs: whether its copy admits a seat now ([Backend.Admits]), and which
// halves it runs natively. A copy that does not is passed over, like a node
// that ran nothing — see [Router.Serves].
//
// NO FLOOR, and past the serving gate on purpose: it asks whether the copy is
// ready for a seat, which is its own stricter gate, not whether it has
// reached this node's writes.
var opPing = define("estate.ping", opRead, address[pingArgs]{partitions: pingPartitions}, false,
	func(ctx context.Context, b Backend, _ *Actor, _ pingArgs) (served, error) {
		if b.Admits != nil && !b.Admits(ctx) {
			return served{}, errNotAdmitting
		}
		return served{Tracker: b.Tracker != nil, Pages: b.Pages != nil}, nil
	}).ungated().floorless()

// ---- custody of a stateless node's event records -------------------------- //

// appendEventsArgs is a batch of records a node holding no store published.
type appendEventsArgs struct {
	Records []store.EventRecord
}

// opAppendEvents takes custody of a stateless node's event records into this
// node's own event log.
//
// A node keeps the record of what it did in its OWN database, written inline
// by whoever published — which a node whose database is deleted at every boot
// cannot keep. So it hands each record to a data node, whose log then holds it
// beside its own, where `GET /events` on that node reads it.
//
// IDEMPOTENT ON THE RECORD'S IDENTITY, which is what makes a repeat after an
// unanswered batch safe: the log's append does nothing for a (time, id) pair
// it already holds. And UNGATED, because a node's own event log is not the
// replicated estate and does not wait for it.
var opAppendEvents = define("events.append", opIdempotentWrite, address[appendEventsArgs]{}, false,
	func(ctx context.Context, b Backend, _ *Actor, a appendEventsArgs) (int, error) {
		if b.Events == nil {
			return 0, errNoHalf
		}
		for i, rec := range a.Records {
			if err := b.Events.Append(ctx, rec); err != nil {
				return i, fmt.Errorf("estate: take custody of event %s: %w", rec.ID, err)
			}
		}
		return len(a.Records), nil
	}).ungated()
