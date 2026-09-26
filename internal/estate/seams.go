package estate

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The asking half: the seams a stateless node's tools are handed, each
// answered by a data node.
//
// Each method has the signature of the in-process one it stands in for, so
// the tool layer is handed these where a data node hands it its own reader
// and writer, and cannot tell — which is the point: a tool that behaved
// differently on a stateless node would be two tools, and only one of them
// tested.

// Work is the tracker's read side.
type Work struct{ c *Client }

// Work answers the tracker's reads.
func (c *Client) Work() Work { return Work{c: c} }

// Tasks answers a board query.
func (w Work) Tasks(ctx context.Context, q tracker.Query, now time.Time) (tracker.Answer, error) {
	return call(ctx, w.c, opTasks, nil, tasksArgs{Query: q, Now: now})
}

// Task answers one task, whole.
func (w Work) Task(ctx context.Context, idOrKey string, want tracker.DetailWants,
	fresh statelog.Freshness) (tracker.TaskDetail, error) {
	return call(ctx, w.c, opTask, nil, taskArgs{IDOrKey: idOrKey, Want: want, Fresh: fresh})
}

// Views answers the saved views.
func (w Work) Views(ctx context.Context, q tracker.ViewQuery) (tracker.ViewListing, error) {
	return call(ctx, w.c, opViews, nil, q)
}

// ExpandedQuery resolves a saved view's parameters into a query.
func (w Work) ExpandedQuery(ctx context.Context, params map[string]any,
	viewer tracker.Viewer, now time.Time, loc *time.Location) (tracker.Query, error) {
	args := expandArgs{Params: params, Viewer: viewer, Now: now}
	if loc != nil {
		args.Zone = loc.String()
	}
	return call(ctx, w.c, opExpandedQuery, nil, args)
}

// Catalogue answers the workspace catalogue.
func (w Work) Catalogue(ctx context.Context, q tracker.CatalogueQuery) (tracker.CatalogueAnswer, error) {
	return call(ctx, w.c, opCatalogue, nil, q)
}

// Person answers one person's own record.
func (w Work) Person(ctx context.Context, q tracker.PersonQuery, now time.Time) (tracker.PersonState, error) {
	return call(ctx, w.c, opPerson, nil, personArgs{Query: q, Now: now})
}

// Thread answers one ask-and-answer thread on a task.
func (w Work) Thread(ctx context.Context, q tracker.ThreadQuery,
	fresh statelog.Freshness) (tracker.ResolvedThread, error) {
	return call(ctx, w.c, opThread, nil, threadArgs{Query: q, Fresh: fresh})
}

// Activity answers what happened to a task or a project.
func (w Work) Activity(ctx context.Context, q tracker.ActivityQuery, now time.Time) (
	tracker.ActivityAnswer, error) {
	return call(ctx, w.c, opActivity, nil, activityArgs{Query: q, Now: now})
}

// MyWork answers what is on one seat's plate.
func (w Work) MyWork(ctx context.Context, q tracker.MyWorkQuery, now time.Time) (tracker.MyWork, error) {
	return call(ctx, w.c, opMyWork, nil, myWorkArgs{Query: q, Now: now})
}

// Projects lists the company's projects.
func (w Work) Projects(ctx context.Context, q tracker.ProjectQuery) (tracker.ProjectListing, error) {
	return call(ctx, w.c, opProjects, nil, q)
}

// Project describes one project.
func (w Work) Project(ctx context.Context, q tracker.ProjectDetailQuery) (tracker.ProjectDetail, error) {
	return call(ctx, w.c, opProject, nil, q)
}

// Search is the tracker's ranked search.
func (w Work) Search(ctx context.Context, text string, limit int) ([]tracker.Ranked, error) {
	return call(ctx, w.c, opWorkSearch, nil, workSearchArgs{Text: text, Limit: limit})
}

// WorkWriter is the tracker's write side, acting as one party.
type WorkWriter struct {
	c     *Client
	actor Actor
}

// WriterAs answers the tracker's writes, attributed to actor.
func (c *Client) WriterAs(actor Actor) WorkWriter { return WorkWriter{c: c, actor: actor} }

// settled raises the session floor to what a write reported, so the next
// read — on whichever node answers it — includes it.
func (w WorkWriter) settled(r statelog.Result) { w.c.Observe(r.Position) }

// CreateTask files a task.
func (w WorkWriter) CreateTask(ctx context.Context, opID string, task tracker.Task,
	notify *tracker.Notify) (tracker.WriteResult, error) {
	out, err := call(ctx, w.c, opCreateTask, &w.actor,
		createTaskArgs{OpID: opID, Task: task, Notify: notify})
	w.settled(out.Result)
	return out, err
}

// UpdateTask patches a task, gestures included.
func (w WorkWriter) UpdateTask(ctx context.Context, opID, id, project string, ifMatch uint64,
	patch tracker.TaskPatch, kind tracker.ChangeKind,
	notify *tracker.Notify) (tracker.WriteResult, error) {
	out, err := call(ctx, w.c, opUpdateTask, &w.actor, updateTaskArgs{
		OpID: opID, ID: id, Project: project, IfMatch: ifMatch, Patch: patch,
		Watch: patch.Watch, Relate: patch.Relate, Depend: patch.Depend, Promote: patch.Promote,
		Kind: kind, Notify: notify,
	})
	w.settled(out.Result)
	return out, err
}

// Depend writes a dependency. The lead map is the SERVING node's — see the
// package doc — so the one passed here does not cross.
func (w WorkWriter) Depend(ctx context.Context, opID string, change tracker.DependencyChange,
	_ tracker.Leads) (tracker.DependencyResult, error) {
	out, err := call(ctx, w.c, opDepend, &w.actor, dependArgs{OpID: opID, Change: change})
	w.settled(out.Result)
	return out, err
}

// MergeDuplicates folds one task into another.
func (w WorkWriter) MergeDuplicates(ctx context.Context, opID, duplicate, into string,
	reparent bool, notify *tracker.Notify) (tracker.WriteResult, error) {
	out, err := call(ctx, w.c, opMergeDuplicates, &w.actor, mergeArgs{
		OpID: opID, Duplicate: duplicate, Into: into, Reparent: reparent, Notify: notify,
	})
	w.settled(out.Result)
	return out, err
}

// MoveTaskToProject moves a task and its subtree to another project.
func (w WorkWriter) MoveTaskToProject(ctx context.Context, opID, taskID, target string,
	notify *tracker.Notify) (tracker.WriteResult, error) {
	out, err := call(ctx, w.c, opMoveTaskToProject, &w.actor, moveArgs{
		OpID: opID, TaskID: taskID, Target: target, Notify: notify,
	})
	w.settled(out.Result)
	return out, err
}

// WriteProject edits a project's own settings.
func (w WorkWriter) WriteProject(ctx context.Context, opID, key string, edit tracker.ProjectEdit,
	authority tracker.ProjectAuthority) (tracker.WriteResult, error) {
	out, err := call(ctx, w.c, opWriteProject, &w.actor, writeProjectArgs{
		OpID: opID, Key: key, Edit: edit, Authority: authority,
	})
	w.settled(out.Result)
	return out, err
}

// WriteTags edits a project's declared tags.
func (w WorkWriter) WriteTags(ctx context.Context, opID, project string, edit tracker.TagEdit,
	authority tracker.TagAuthority) (tracker.WriteResult, error) {
	out, err := call(ctx, w.c, opWriteTags, &w.actor, writeTagsArgs{
		OpID: opID, Project: project, Edit: edit, Authority: authority,
	})
	w.settled(out.Result)
	return out, err
}

// EnsureTags declares the tags a write is about to use.
func (w WorkWriter) EnsureTags(ctx context.Context, opID, project string, tags []string) (
	[]string, []string, error) {
	out, err := call(ctx, w.c, opEnsureTags, &w.actor, ensureTagsArgs{
		OpID: opID, Project: project, Tags: tags,
	})
	return out.Created, out.Warnings, err
}

// Pages is the knowledge base, read and written.
type Pages struct{ c *Client }

// Pages answers the knowledge base.
func (c *Client) Pages() Pages { return Pages{c: c} }

// List answers a listing of pages.
func (p Pages) List(ctx context.Context, f pages.Filter, fresh statelog.Freshness) (pages.Listing, error) {
	return call(ctx, p.c, opListPages, nil, listPagesArgs{Filter: f, Fresh: fresh})
}

// Get answers one page.
func (p Pages) Get(ctx context.Context, ref string, fresh statelog.Freshness) (pages.Detail, error) {
	return call(ctx, p.c, opGetPage, nil, getPageArgs{Ref: ref, Fresh: fresh})
}

// SkillPages is the tool-skill container, for the skill sync.
func (p Pages) SkillPages(ctx context.Context, container string,
	fresh statelog.Freshness) ([]pages.Page, error) {
	return call(ctx, p.c, opSkillPages, nil, skillPagesArgs{Container: container, Fresh: fresh})
}

// Create writes a new page. Never repeated when unanswered — see [ErrOutcomeUnknown].
func (p Pages) Create(ctx context.Context, actor pages.Actor, in pages.NewPage) (pages.Written, error) {
	out, err := call(ctx, p.c, opCreatePage, nil, createPageArgs{Actor: actor, Page: in})
	p.c.Observe(out.Outcome.Position)
	return out, err
}

// SavePage saves a page's body. Never repeated when unanswered.
func (p Pages) SavePage(ctx context.Context, actor pages.Actor, pageID string,
	save pages.Save) (pages.Written, error) {
	out, err := call(ctx, p.c, opSavePage, nil, savePageArgs{Actor: actor, PageID: pageID, Save: save})
	p.c.Observe(out.Outcome.Position)
	return out, err
}

// Rename moves a page to a new title. Never repeated when unanswered.
func (p Pages) Rename(ctx context.Context, actor pages.Actor, pageID, title string,
	quiet bool) (pages.Written, error) {
	out, err := call(ctx, p.c, opRenamePage, nil, renamePageArgs{
		Actor: actor, PageID: pageID, Title: title, Quiet: quiet,
	})
	p.c.Observe(out.Outcome.Position)
	return out, err
}

// Comment adds a comment. Never repeated when unanswered.
func (p Pages) Comment(ctx context.Context, actor pages.Actor, pageID string,
	in pages.NewComment) (pages.Comment, pages.Written, error) {
	out, err := call(ctx, p.c, opCommentPage, nil, commentArgs{Actor: actor, PageID: pageID, Comment: in})
	p.c.Observe(out.Written.Outcome.Position)
	return out.Comment, out.Written, err
}

// EditComment rewrites a comment. Never repeated when unanswered.
func (p Pages) EditComment(ctx context.Context, actor pages.Actor, pageID, commentID,
	body string) (pages.Comment, pages.Written, error) {
	out, err := call(ctx, p.c, opEditComment, nil, editCommentArgs{
		Actor: actor, PageID: pageID, CommentID: commentID, Body: body,
	})
	p.c.Observe(out.Written.Outcome.Position)
	return out.Comment, out.Written, err
}

// Knowledge is the native knowledge search, answered by a data node.
type Knowledge struct{ c *Client }

// Knowledge answers the knowledge search.
func (c *Client) Knowledge() Knowledge { return Knowledge{c: c} }

var _ knowledge.Searcher = Knowledge{}

// Backend is the native one: a stateless node searches the engine's own
// knowledge base, through a node that holds it.
func (Knowledge) Backend() string { return "native" }

// CanSearch is the no-I/O pre-gate, and on the native backend every seat can
// read every page — so it is true, and a data node that answers nothing is
// the empty result a best-effort search already is.
func (Knowledge) CanSearch(*org.Role, *org.Organization) bool { return true }

// Search is BEST EFFORT, as the seam requires: a failure is logged and
// answers empty, because a turn must not die because a data node was slow.
func (k Knowledge) Search(ctx context.Context, q knowledge.Query) []knowledge.Hit {
	args := knowledgeArgs{Text: q.Text, Limit: q.Limit, Scoped: q.Org != nil}
	if q.Seat != nil {
		args.Seat = q.Seat.Handle()
	}
	if q.ExcludeAncestors != nil {
		args.Exclusion, args.ExcludeAncestors = true, q.ExcludeAncestors
	}
	hits, err := call(ctx, k.c, opKnowledgeSearch, nil, args)
	if err != nil {
		log.WarnContext(ctx, "knowledge_search_remote_failed", "error", err.Error(),
			"detail", "the knowledge block degrades to empty; a turn must not die "+
				"because a data node was slow")
		return nil
	}
	return hits
}

// Building reports whether the answering node's index is still building,
// which is what turns an empty block into "still indexing" rather than "the
// company has written nothing down". False when nothing answers.
func (k Knowledge) Building(ctx context.Context) bool {
	building, err := call(ctx, k.c, opKnowledgeBuilding, nil, struct{}{})
	return err == nil && building
}

// Serves reports what the fleet's data nodes run, from the first that
// answers — the question a stateless node's seat admission asks: is there a
// data node at all, and is it ready for a seat's tools.
func (c *Client) Serves(ctx context.Context) (tracker, pages bool, err error) {
	out, err := call(ctx, c, opPing, nil, struct{}{})
	return out.Tracker, out.Pages, err
}

// AppendEvents hands a batch of this node's event records to a data node's
// event log — see [opAppendEvents]. It answers how many were taken.
func (c *Client) AppendEvents(ctx context.Context, records []store.EventRecord) (int, error) {
	return call(ctx, c, opAppendEvents, nil, appendEventsArgs{Records: records})
}
