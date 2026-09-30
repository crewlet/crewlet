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

// The asking half: the seams every node's tools are handed, each answered by
// whichever node serves the partition the call addresses — this one where it
// does.
//
// Each method has the signature of the in-process one it stands in for, so
// the tool layer is handed these on every node and cannot tell which node
// answered — which is the point: a tool that behaved differently on a node
// holding no data would be two tools, and only one of them tested.

// Work is the tracker's read side.
type Work struct{ r *Router }

// Work answers the tracker's reads.
func (r *Router) Work() Work { return Work{r: r} }

// Tasks answers a board query.
func (w Work) Tasks(ctx context.Context, q tracker.Query, now time.Time) (tracker.Answer, error) {
	return call(ctx, w.r, opTasks, nil, tasksArgs{Query: q, Now: now})
}

// Task answers one task, whole.
func (w Work) Task(ctx context.Context, idOrKey string, want tracker.DetailWants,
	fresh statelog.Freshness) (tracker.TaskDetail, error) {
	return call(ctx, w.r, opTask, nil, taskArgs{IDOrKey: idOrKey, Want: want, Fresh: fresh})
}

// Files answers a page of a project's files.
func (w Work) Files(ctx context.Context, q tracker.FileQuery) (tracker.FileListing, error) {
	return call(ctx, w.r, opFiles, nil, q)
}

// File answers one file with its manifest.
func (w Work) File(ctx context.Context, project, path string,
	fresh statelog.Freshness) (tracker.FileDetail, error) {
	return call(ctx, w.r, opFile, nil, fileArgs{Project: project, Path: path, Fresh: fresh})
}

// Views answers the saved views.
func (w Work) Views(ctx context.Context, q tracker.ViewQuery) (tracker.ViewListing, error) {
	return call(ctx, w.r, opViews, nil, q)
}

// ExpandedQuery resolves a saved view's parameters into a query.
func (w Work) ExpandedQuery(ctx context.Context, params map[string]any,
	viewer tracker.Viewer, now time.Time, loc *time.Location) (tracker.Query, error) {
	args := expandArgs{Params: params, Viewer: viewer, Now: now}
	if loc != nil {
		args.Zone = loc.String()
	}
	return call(ctx, w.r, opExpandedQuery, nil, args)
}

// Catalogue answers the workspace catalogue.
func (w Work) Catalogue(ctx context.Context, q tracker.CatalogueQuery) (tracker.CatalogueAnswer, error) {
	return call(ctx, w.r, opCatalogue, nil, q)
}

// Person answers one person's own record.
func (w Work) Person(ctx context.Context, q tracker.PersonQuery, now time.Time) (tracker.PersonState, error) {
	return call(ctx, w.r, opPerson, nil, personArgs{Query: q, Now: now})
}

// Thread answers one ask-and-answer thread on a task.
func (w Work) Thread(ctx context.Context, q tracker.ThreadQuery,
	fresh statelog.Freshness) (tracker.ResolvedThread, error) {
	return call(ctx, w.r, opThread, nil, threadArgs{Query: q, Fresh: fresh})
}

// Activity answers what happened to a task or a project.
func (w Work) Activity(ctx context.Context, q tracker.ActivityQuery, now time.Time) (
	tracker.ActivityAnswer, error) {
	return call(ctx, w.r, opActivity, nil, activityArgs{Query: q, Now: now})
}

// MyWork answers what is on one seat's plate.
func (w Work) MyWork(ctx context.Context, q tracker.MyWorkQuery, now time.Time) (tracker.MyWork, error) {
	return call(ctx, w.r, opMyWork, nil, myWorkArgs{Query: q, Now: now})
}

// Projects lists the company's projects.
func (w Work) Projects(ctx context.Context, q tracker.ProjectQuery) (tracker.ProjectListing, error) {
	return call(ctx, w.r, opProjects, nil, q)
}

// Project describes one project.
func (w Work) Project(ctx context.Context, q tracker.ProjectDetailQuery) (tracker.ProjectDetail, error) {
	return call(ctx, w.r, opProject, nil, q)
}

// Search is the tracker's ranked search.
func (w Work) Search(ctx context.Context, text string, limit int) ([]tracker.Ranked, error) {
	return call(ctx, w.r, opWorkSearch, nil, workSearchArgs{Text: text, Limit: limit})
}

// WorkWriter is the tracker's write side, acting as one party.
type WorkWriter struct {
	r     *Router
	actor Actor
}

// WriterAs answers the tracker's writes, attributed to actor.
func (r *Router) WriterAs(actor Actor) WorkWriter { return WorkWriter{r: r, actor: actor} }

// settled raises the session floor to what a write reported, so the next
// read — on whichever node answers it, this one included — includes it.
func (w WorkWriter) settled(res statelog.Result) { w.r.Observe(res.Position) }

// CreateTask files a task.
func (w WorkWriter) CreateTask(ctx context.Context, opID string, task tracker.Task,
	notify *tracker.Notify) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opCreateTask, &w.actor,
		createTaskArgs{OpID: opID, Task: task, Notify: notify})
	w.settled(out.Result)
	return out, err
}

// UpdateTask patches a task, gestures included.
func (w WorkWriter) UpdateTask(ctx context.Context, opID, id, project string, ifMatch uint64,
	patch tracker.TaskPatch, kind tracker.ChangeKind,
	notify *tracker.Notify) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opUpdateTask, &w.actor, updateTaskArgs{
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
	out, err := call(ctx, w.r, opDepend, &w.actor, dependArgs{OpID: opID, Change: change})
	w.settled(out.Result)
	return out, err
}

// MergeDuplicates folds one task into another.
func (w WorkWriter) MergeDuplicates(ctx context.Context, opID, duplicate, into string,
	reparent bool, notify *tracker.Notify) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opMergeDuplicates, &w.actor, mergeArgs{
		OpID: opID, Duplicate: duplicate, Into: into, Reparent: reparent, Notify: notify,
	})
	w.settled(out.Result)
	return out, err
}

// MoveTaskToProject moves a task and its subtree to another project.
func (w WorkWriter) MoveTaskToProject(ctx context.Context, opID, taskID, target string,
	notify *tracker.Notify) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opMoveTaskToProject, &w.actor, moveArgs{
		OpID: opID, TaskID: taskID, Target: target, Notify: notify,
	})
	w.settled(out.Result)
	return out, err
}

// WriteProject edits a project's own settings.
func (w WorkWriter) WriteProject(ctx context.Context, opID, key string, edit tracker.ProjectEdit,
	authority tracker.ProjectAuthority) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opWriteProject, &w.actor, writeProjectArgs{
		OpID: opID, Key: key, Edit: edit, Authority: authority,
	})
	w.settled(out.Result)
	return out, err
}

// WriteTags edits a project's declared tags.
func (w WorkWriter) WriteTags(ctx context.Context, opID, project string, edit tracker.TagEdit,
	authority tracker.TagAuthority) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opWriteTags, &w.actor, writeTagsArgs{
		OpID: opID, Project: project, Edit: edit, Authority: authority,
	})
	w.settled(out.Result)
	return out, err
}

// EnsureTags declares the tags a write is about to use.
func (w WorkWriter) EnsureTags(ctx context.Context, opID, project string, tags []string) (
	[]string, []string, error) {
	out, err := call(ctx, w.r, opEnsureTags, &w.actor, ensureTagsArgs{
		OpID: opID, Project: project, Tags: tags,
	})
	return out.Created, out.Warnings, err
}

// PutFile writes a project's file — its row, naming chunks the caller has
// already uploaded through its own object client.
func (w WorkWriter) PutFile(ctx context.Context, opID string, put tracker.FilePut) (
	tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opPutFile, &w.actor, putFileArgs{OpID: opID, Put: put})
	w.settled(out.Result)
	return out, err
}

// RemoveFile takes a file out of its project.
func (w WorkWriter) RemoveFile(ctx context.Context, opID, project, path string,
	ifMatch uint64) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opRemoveFile, &w.actor, removeFileArgs{
		OpID: opID, Project: project, Path: path, IfMatch: ifMatch,
	})
	w.settled(out.Result)
	return out, err
}

// RecordTurn records one turn's spend on the task it worked on.
func (w WorkWriter) RecordTurn(ctx context.Context, opID string,
	turn tracker.TurnRecord) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opRecordTurn, &w.actor, recordTurnArgs{OpID: opID, Turn: turn})
	w.settled(out.Result)
	return out, err
}

// Pages is the knowledge base, read and written.
type Pages struct{ r *Router }

// Pages answers the knowledge base.
func (r *Router) Pages() Pages { return Pages{r: r} }

// List answers a listing of pages.
func (p Pages) List(ctx context.Context, f pages.Filter, fresh statelog.Freshness) (pages.Listing, error) {
	return call(ctx, p.r, opListPages, nil, listPagesArgs{Filter: f, Fresh: fresh})
}

// Get answers one page.
func (p Pages) Get(ctx context.Context, ref string, fresh statelog.Freshness) (pages.Detail, error) {
	return call(ctx, p.r, opGetPage, nil, getPageArgs{Ref: ref, Fresh: fresh})
}

// SkillPages is the tool-skill container, for the skill sync.
func (p Pages) SkillPages(ctx context.Context, container string,
	fresh statelog.Freshness) ([]pages.Page, error) {
	return call(ctx, p.r, opSkillPages, nil, skillPagesArgs{Container: container, Fresh: fresh})
}

// Create writes a new page. Never repeated when unanswered — see [ErrOutcomeUnknown].
func (p Pages) Create(ctx context.Context, actor pages.Actor, in pages.NewPage) (pages.Written, error) {
	out, err := call(ctx, p.r, opCreatePage, nil, createPageArgs{Actor: actor, Page: in})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// SavePage saves a page's body. Never repeated when unanswered.
func (p Pages) SavePage(ctx context.Context, actor pages.Actor, pageID string,
	save pages.Save) (pages.Written, error) {
	out, err := call(ctx, p.r, opSavePage, nil, savePageArgs{Actor: actor, PageID: pageID, Save: save})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// Rename moves a page to a new title. Never repeated when unanswered.
func (p Pages) Rename(ctx context.Context, actor pages.Actor, pageID, title string,
	quiet bool) (pages.Written, error) {
	out, err := call(ctx, p.r, opRenamePage, nil, renamePageArgs{
		Actor: actor, PageID: pageID, Title: title, Quiet: quiet,
	})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// Comment adds a comment. Never repeated when unanswered.
func (p Pages) Comment(ctx context.Context, actor pages.Actor, pageID string,
	in pages.NewComment) (pages.Comment, pages.Written, error) {
	out, err := call(ctx, p.r, opCommentPage, nil, commentArgs{Actor: actor, PageID: pageID, Comment: in})
	p.r.Observe(out.Written.Outcome.Position)
	return out.Comment, out.Written, err
}

// EditComment rewrites a comment. Never repeated when unanswered.
func (p Pages) EditComment(ctx context.Context, actor pages.Actor, pageID, commentID,
	body string) (pages.Comment, pages.Written, error) {
	out, err := call(ctx, p.r, opEditComment, nil, editCommentArgs{
		Actor: actor, PageID: pageID, CommentID: commentID, Body: body,
	})
	p.r.Observe(out.Written.Outcome.Position)
	return out.Comment, out.Written, err
}

// Knowledge is the native knowledge search, answered by a node that holds the
// index.
type Knowledge struct{ r *Router }

// Knowledge answers the knowledge search.
func (r *Router) Knowledge() Knowledge { return Knowledge{r: r} }

var _ knowledge.Searcher = Knowledge{}

// Backend is the native one: the engine's own knowledge base, searched on a
// node that holds it.
func (Knowledge) Backend() string { return "native" }

// CanSearch is the no-I/O pre-gate, and on the native backend every seat can
// read every page — so it is true, and a data node that answers nothing is
// the empty result a best-effort search already is.
func (Knowledge) CanSearch(*org.Role, *org.Organization) bool { return true }

// Search is BEST EFFORT, as the seam requires: a failure is logged and
// answers empty, because a turn must not die because the node holding the
// index — this one or another — was slow.
func (k Knowledge) Search(ctx context.Context, q knowledge.Query) []knowledge.Hit {
	args := knowledgeArgs{Text: q.Text, Limit: q.Limit, Scoped: q.Org != nil}
	if q.Seat != nil {
		args.Seat = q.Seat.Handle()
	}
	if q.ExcludeAncestors != nil {
		args.Exclusion, args.ExcludeAncestors = true, q.ExcludeAncestors
	}
	hits, err := call(ctx, k.r, opKnowledgeSearch, nil, args)
	if err != nil {
		log.WarnContext(ctx, "knowledge_search_failed", "error", err.Error(),
			"detail", "the knowledge block degrades to empty; a turn must not die "+
				"because the node holding the index was slow")
		return nil
	}
	return hits
}

// Building reports whether the answering node's index is still building,
// which is what turns an empty block into "still indexing" rather than "the
// company has written nothing down". False when nothing answers.
func (k Knowledge) Building(ctx context.Context) bool {
	building, err := call(ctx, k.r, opKnowledgeBuilding, nil, struct{}{})
	return err == nil && building
}

// AppendEvents hands a batch of this node's event records to a data node's
// event log — see [opAppendEvents]. It answers how many were taken.
func (r *Router) AppendEvents(ctx context.Context, records []store.EventRecord) (int, error) {
	return call(ctx, r, opAppendEvents, nil, appendEventsArgs{Records: records})
}
