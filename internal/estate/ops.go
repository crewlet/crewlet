package estate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The serving half: what a data node answers with.
//
// Declared here, by the consumer, and kept to exactly what the operations
// below call — which is the seat surface's tools and nothing more. The
// operator's own surfaces (views, the catalogue, a person's inbox, the trash)
// are served by the API, and the API runs only where the data is.

// TrackerReader is the tracker's read side a seat's tools ask of.
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
}

// WorkSearcher is the tracker's ranked search.
type WorkSearcher interface {
	Search(ctx context.Context, text string, limit int) ([]tracker.Ranked, error)
}

// PageReader is the knowledge base's read side.
type PageReader interface {
	List(ctx context.Context, f pages.Filter, fresh statelog.Freshness) (pages.Listing, error)
	Get(ctx context.Context, ref string, fresh statelog.Freshness) (pages.Detail, error)
	SkillPages(ctx context.Context, container string, fresh statelog.Freshness) ([]pages.Page, error)
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

// KnowledgeSearcher is the native knowledge search, with the one question a
// caller asks of an empty answer.
type KnowledgeSearcher interface {
	Search(ctx context.Context, q knowledge.Query) []knowledge.Hit
	Building(ctx context.Context) bool
}

// Backend is one serving node's answer to every operation, resolved PER
// REQUEST: a node that booted with no company brings its native runtime up
// at its first apply, and a request that arrives before then is answered
// "not here" rather than refused for ever.
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

	// Units is attached to every tracker read that renders a project's
	// unit, and Leads is handed to a dependency — the SERVING node's chart,
	// which is what the package doc says crosses no wire.
	Units tracker.Units
	Leads tracker.Leads

	// Seat resolves a searching seat's handle to its role and the org its
	// read scope comes from, against this node's current epoch.
	Seat func(handle string) (*org.Role, *org.Organization)

	// Committed waits until this node's applier holds a position — the
	// floor every request carries.
	Committed func(ctx context.Context, at statelog.Position) error

	// Established is the gate seats are admitted by: a node whose copy is
	// not one a seat's tools may read answers nothing.
	Established func(ctx context.Context) bool

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

// The streams a floor names.
const (
	trackerStream = topics.TrackerLogStream
	pagesStream   = topics.PagesLogStream
)

// ---- the tracker's reads ------------------------------------------------ //

type tasksArgs struct {
	Query tracker.Query
	Now   time.Time
}

var opTasks = define("tracker.tasks", opRead, trackerStream, false,
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
}

var opTask = define("tracker.task", opRead, trackerStream, false,
	func(ctx context.Context, b Backend, _ *Actor, a taskArgs) (tracker.TaskDetail, error) {
		if b.Tracker == nil {
			return tracker.TaskDetail{}, errNoHalf
		}
		a.Want.Units = b.Units
		return b.Tracker.Task(ctx, a.IDOrKey, a.Want, a.Fresh)
	})

var opViews = define("tracker.views", opRead, trackerStream, false,
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

var opExpandedQuery = define("tracker.expanded_query", opRead, trackerStream, false,
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

var opCatalogue = define("tracker.catalogue", opRead, trackerStream, false,
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

var opPerson = define("tracker.person", opRead, trackerStream, false,
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

var opThread = define("tracker.thread", opRead, trackerStream, false,
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

var opActivity = define("tracker.activity", opRead, trackerStream, false,
	func(ctx context.Context, b Backend, _ *Actor, a activityArgs) (tracker.ActivityAnswer, error) {
		if b.Tracker == nil {
			return tracker.ActivityAnswer{}, errNoHalf
		}
		return b.Tracker.Activity(ctx, a.Query, a.Now)
	})

type myWorkArgs struct {
	Query tracker.MyWorkQuery
	Now   time.Time
}

var opMyWork = define("tracker.my_work", opRead, trackerStream, false,
	func(ctx context.Context, b Backend, _ *Actor, a myWorkArgs) (tracker.MyWork, error) {
		if b.Tracker == nil {
			return tracker.MyWork{}, errNoHalf
		}
		return b.Tracker.MyWork(ctx, a.Query, a.Now)
	})

var opProjects = define("tracker.projects", opRead, trackerStream, false,
	func(ctx context.Context, b Backend, _ *Actor, q tracker.ProjectQuery) (tracker.ProjectListing, error) {
		if b.Tracker == nil {
			return tracker.ProjectListing{}, errNoHalf
		}
		q.Units = b.Units
		return b.Tracker.Projects(ctx, q)
	})

var opProject = define("tracker.project", opRead, trackerStream, false,
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

// NO FLOOR: the ranked search reads the lexical index, which each node's
// own walk maintains on its own schedule behind the applier, so a floor on
// the log would promise a visibility the index does not give.
var opWorkSearch = define("tracker.search", opRead, "", false,
	func(ctx context.Context, b Backend, _ *Actor, a workSearchArgs) ([]tracker.Ranked, error) {
		if b.WorkSearch == nil {
			return nil, errNoHalf
		}
		return b.WorkSearch.Search(ctx, a.Text, a.Limit)
	})

// ---- the tracker's writes ------------------------------------------------ //

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

var opCreateTask = define("tracker.create_task", opIdempotentWrite, trackerStream, true,
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

var opUpdateTask = define("tracker.update_task", opIdempotentWrite, trackerStream, true,
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

var opDepend = define("tracker.depend", opIdempotentWrite, trackerStream, true,
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

var opMergeDuplicates = define("tracker.merge_duplicates", opIdempotentWrite, trackerStream, true,
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

var opMoveTaskToProject = define("tracker.move_task_to_project", opIdempotentWrite, trackerStream, true,
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

var opWriteProject = define("tracker.write_project", opIdempotentWrite, trackerStream, true,
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

var opWriteTags = define("tracker.write_tags", opIdempotentWrite, trackerStream, true,
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

var opEnsureTags = define("tracker.ensure_tags", opIdempotentWrite, trackerStream, true,
	func(ctx context.Context, b Backend, actor *Actor, a ensureTagsArgs) (ensuredTags, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return ensuredTags{}, err
		}
		created, warnings, err := w.EnsureTags(ctx, a.OpID, a.Project, a.Tags)
		return ensuredTags{Created: created, Warnings: warnings}, err
	})

// ---- the knowledge base -------------------------------------------------- //

type listPagesArgs struct {
	Filter pages.Filter
	Fresh  statelog.Freshness
}

var opListPages = define("pages.list", opRead, pagesStream, false,
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

var opGetPage = define("pages.get", opRead, pagesStream, false,
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

var opSkillPages = define("pages.skill_pages", opRead, pagesStream, false,
	func(ctx context.Context, b Backend, _ *Actor, a skillPagesArgs) ([]pages.Page, error) {
		if b.Pages == nil {
			return nil, errNoHalf
		}
		return b.Pages.SkillPages(ctx, a.Container, a.Fresh)
	})

type createPageArgs struct {
	Actor pages.Actor
	Page  pages.NewPage
}

var opCreatePage = define("pages.create", opOnceWrite, pagesStream, false,
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

var opSavePage = define("pages.save", opOnceWrite, pagesStream, false,
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

var opRenamePage = define("pages.rename", opOnceWrite, pagesStream, false,
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

var opCommentPage = define("pages.comment", opOnceWrite, pagesStream, false,
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

var opEditComment = define("pages.edit_comment", opOnceWrite, pagesStream, false,
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

var opKnowledgeSearch = define("knowledge.search", opRead, "", false,
	func(ctx context.Context, b Backend, _ *Actor, a knowledgeArgs) ([]knowledge.Hit, error) {
		if b.Knowledge == nil {
			return nil, errNoHalf
		}
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
		return b.Knowledge.Search(ctx, q), nil
	})

var opKnowledgeBuilding = define("knowledge.building", opRead, "", false,
	func(ctx context.Context, b Backend, _ *Actor, _ struct{}) (bool, error) {
		if b.Knowledge == nil {
			return false, errNoHalf
		}
		return b.Knowledge.Building(ctx), nil
	})

// served is what a node runs, as a stateless node's admission asks it.
type served struct {
	Tracker bool
	Pages   bool
}

var opPing = define("estate.ping", opRead, "", false,
	func(_ context.Context, b Backend, _ *Actor, _ struct{}) (served, error) {
		return served{Tracker: b.Tracker != nil, Pages: b.Pages != nil}, nil
	})

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
var opAppendEvents = defineUngated("events.append", opIdempotentWrite, "", false,
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
	})
