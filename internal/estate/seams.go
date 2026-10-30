package estate

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
	"github.com/crewlet/crewlet/internal/usage"
)

// The asking half: the seams every node's tools are handed, each answered by a
// data node whose copy of the operation's domain serves — this one where it
// does (ADR-0049).
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
	args := taskArgs{IDOrKey: idOrKey, Want: want, Fresh: fresh}
	if want.Clock != nil {
		args.Zone = zoneArg(want.Clock.Zone)
	}
	return call(ctx, w.r, opTask, nil, args)
}

// Files answers a page of a project's files.
func (w Work) Files(ctx context.Context, q tracker.FileQuery) (tracker.FileListing, error) {
	return call(ctx, w.r, opFiles, nil, q)
}

// File answers one file with the object holding its bytes.
func (w Work) File(ctx context.Context, project, path string,
	fresh statelog.Freshness) (tracker.FileDetail, error) {
	return call(ctx, w.r, opFile, nil, fileArgs{Project: project, Path: path, Fresh: fresh})
}

// Views answers the saved views.
func (w Work) Views(ctx context.Context, q tracker.ViewQuery) (tracker.ViewListing, error) {
	return call(ctx, w.r, opViews, nil, viewsArgs{Query: q, Zone: zoneArg(q.Zone)})
}

// ExpandedQuery resolves a saved view's parameters into a query.
func (w Work) ExpandedQuery(ctx context.Context, params map[string]any,
	viewer tracker.Viewer, now time.Time, loc *time.Location) (tracker.Query, error) {
	return call(ctx, w.r, opExpandedQuery, nil,
		expandArgs{Params: params, Viewer: viewer, Now: now, Zone: zoneArg(loc)})
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

// MyWork answers what is on one seat's plate, "today" cut on loc.
func (w Work) MyWork(ctx context.Context, q tracker.MyWorkQuery, now time.Time,
	loc *time.Location) (tracker.MyWork, error) {
	return call(ctx, w.r, opMyWork, nil, myWorkArgs{Query: q, Now: now, Zone: zoneArg(loc)})
}

// Projects lists the company's projects.
func (w Work) Projects(ctx context.Context, q tracker.ProjectQuery) (tracker.ProjectListing, error) {
	return call(ctx, w.r, opProjects, nil, q)
}

// Project describes one project.
func (w Work) Project(ctx context.Context, q tracker.ProjectDetailQuery) (tracker.ProjectDetail, error) {
	return call(ctx, w.r, opProject, nil, q)
}

// Search is the tracker's ranked search, answered by a data node's index. A
// search no node could run is its error, never an empty answer: "nothing
// matched" would send the seat off to file what it was looking for.
func (w Work) Search(ctx context.Context, q tracker.SearchQuery) (tracker.SearchAnswer, error) {
	if !q.Mode.Valid() {
		return tracker.SearchAnswer{}, fmt.Errorf("tracker: unknown search mode %q — "+
			"the modes are hybrid, keyword and semantic", q.Mode)
	}
	return call(ctx, w.r, opWorkSearch, nil, workSearchArgs{Text: q.Text, Limit: q.Limit, Mode: q.Mode})
}

// Workload answers a unit's workload, its days cut on loc.
func (w Work) Workload(ctx context.Context, q tracker.WorkloadQuery, now time.Time,
	loc *time.Location) (tracker.WorkloadAnswer, error) {
	return call(ctx, w.r, opWorkload, nil, workloadArgs{Query: q, Now: now, Zone: zoneArg(loc)})
}

// Inbox answers what the company has asked of one person.
func (w Work) Inbox(ctx context.Context, q tracker.InboxQuery, now time.Time) (
	tracker.InboxAnswer, error) {
	return call(ctx, w.r, opInbox, nil, inboxArgs{Query: q, Now: now})
}

// Routing answers who a change reached, and why.
func (w Work) Routing(ctx context.Context, q tracker.RoutingQuery, now time.Time) (
	tracker.RoutingAnswer, error) {
	return call(ctx, w.r, opRouting, nil, routingArgs{Query: q, Now: now})
}

// TurnsOf answers a page of the turns charged to one task.
func (w Work) TurnsOf(ctx context.Context, idOrKey, cursor string, limit int,
	fresh statelog.Freshness) (tracker.TaskTurns, error) {
	return call(ctx, w.r, opTurnsOf, nil,
		turnsOfArgs{IDOrKey: idOrKey, Cursor: cursor, Limit: limit, Fresh: fresh})
}

// EveryView answers every saved view, whoever owns it.
func (w Work) EveryView(ctx context.Context, q tracker.EveryViewQuery) (tracker.ViewListing, error) {
	return call(ctx, w.r, opEveryView, nil, everyViewArgs{Query: q, Zone: zoneArg(q.Zone)})
}

// Flow answers how work moved through the company, its days cut on loc.
func (w Work) Flow(ctx context.Context, q tracker.FlowQuery, now time.Time,
	loc *time.Location) (tracker.FlowAnswer, error) {
	return call(ctx, w.r, opFlow, nil, flowArgs{Query: q, Now: now, Zone: zoneArg(loc)})
}

// CompanyFeed answers a page of the company's feed.
func (w Work) CompanyFeed(ctx context.Context, q tracker.FeedQuery) (tracker.FeedPage, error) {
	return call(ctx, w.r, opCompanyFeed, nil, q)
}

// Decisions answers the structured asks waiting on a decision, "overdue" cut
// on loc.
func (w Work) Decisions(ctx context.Context, q tracker.DecisionsQuery, now time.Time,
	loc *time.Location) (tracker.DecisionsAnswer, error) {
	return call(ctx, w.r, opDecisions, nil, decisionsArgs{Query: q, Now: now, Zone: zoneArg(loc)})
}

// TurnPlaces answers which task each run was charged to.
func (w Work) TurnPlaces(ctx context.Context, runs []string,
	fresh statelog.Freshness) (map[string]tracker.TurnPlace, error) {
	return call(ctx, w.r, opTurnPlaces, nil, turnPlacesArgs{Runs: runs, Fresh: fresh})
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

// CreateTaskAsking files a task with a structured ask on it, in one gesture.
func (w WorkWriter) CreateTaskAsking(ctx context.Context, opID string, task tracker.Task,
	ask tracker.Comment, notify *tracker.Notify) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opCreateTaskAsking, &w.actor,
		createTaskAskingArgs{OpID: opID, Task: task, Ask: ask, Notify: notify})
	w.settled(out.Result)
	return out, err
}

// PlaceTask drops a card on the board: its lane, then its place.
func (w WorkWriter) PlaceTask(ctx context.Context, opID string, place tracker.Place,
	notify *tracker.Notify) (tracker.PlaceResult, error) {
	out, err := call(ctx, w.r, opPlaceTask, &w.actor,
		placeTaskArgs{OpID: opID, Place: place, Notify: notify})
	res := out.Result
	res.Unplaced = decodeError(out.Unplaced)
	w.settled(res.Lane.Result)
	w.settled(res.Order.Result)
	return res, err
}

// UpdateTask patches a task, gestures included.
func (w WorkWriter) UpdateTask(ctx context.Context, opID, id, project string, ifMatch uint64,
	patch tracker.TaskPatch, kind tracker.ChangeKind,
	notify *tracker.Notify) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opUpdateTask, &w.actor, updateTaskArgs{
		OpID: opID, ID: id, Project: project, IfMatch: ifMatch, Patch: patch,
		Watch: patch.Watch, Relate: patch.Relate, Depend: patch.Depend,
		Checklist: patch.Checklist, Kind: kind, Notify: notify,
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

// PutFile writes a project's file — its row, naming the object the caller has
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

// ViewPrior reads what a save of view id would replace, for the caller to
// decide its authority on and hand back to [WorkWriter.WriteView].
func (w WorkWriter) ViewPrior(ctx context.Context, id string) (tracker.ViewPrior, error) {
	return call(ctx, w.r, opViewPrior, &w.actor, viewPriorArgs{ID: id})
}

// WriteView saves a view, refused where the stored view is no longer prior.
func (w WorkWriter) WriteView(ctx context.Context, opID string, view tracker.View,
	prior tracker.ViewPrior) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opWriteView, &w.actor,
		writeViewArgs{OpID: opID, View: view, Prior: prior})
	w.settled(out.Result)
	return out, err
}

// WriteTypes sets the company's task types.
func (w WorkWriter) WriteTypes(ctx context.Context, opID string, types []tracker.TaskType) (
	tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opWriteTypes, &w.actor, writeTypesArgs{OpID: opID, Types: types})
	w.settled(out.Result)
	return out, err
}

// WriteFields sets the company's custom fields.
func (w WorkWriter) WriteFields(ctx context.Context, opID string, fields []tracker.FieldDef) (
	tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opWriteFields, &w.actor, writeFieldsArgs{OpID: opID, Fields: fields})
	w.settled(out.Result)
	return out, err
}

// MarkInbox applies one gesture to a person's inbox, under the authority the
// caller's table decided.
func (w WorkWriter) MarkInbox(ctx context.Context, opID, handle string,
	gesture inbox.Gesture[tracker.Reason], authority tracker.PersonAuthority) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opMarkInbox, &w.actor, markInboxArgs{
		OpID: opID, Handle: handle, Gesture: gesture, Authority: authority,
	})
	w.settled(out.Result)
	return out, err
}

// WritePins applies one gesture to a person's pinned views and favourites,
// under the authority the caller's table decided.
func (w WorkWriter) WritePins(ctx context.Context, opID, handle string,
	gesture tracker.PinGesture, authority tracker.PersonAuthority) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opWritePins, &w.actor, writePinsArgs{
		OpID: opID, Handle: handle, Gesture: gesture, Authority: authority,
	})
	w.settled(out.Result)
	return out, err
}

// EditComment rewrites the writer's own remark on a work item.
func (w WorkWriter) EditComment(ctx context.Context, opID, taskID, project, commentID,
	body string, notify *tracker.Notify) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opEditTaskComment, &w.actor, editCommentOnTaskArgs{
		OpID: opID, TaskID: taskID, Project: project, CommentID: commentID, Body: body,
		Notify: notify,
	})
	w.settled(out.Result)
	return out, err
}

// WritePriorities sets a person's priority order, conditioned on ifMatch.
func (w WorkWriter) WritePriorities(ctx context.Context, opID, handle string,
	priorities []string, ifMatch *uint64, authority tracker.PersonAuthority) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opWritePriorities, &w.actor, writePrioritiesArgs{
		OpID: opID, Handle: handle, Priorities: priorities, IfMatch: ifMatch, Authority: authority,
	})
	w.settled(out.Result)
	return out, err
}

// RemoveTask moves a task, or its subtree, to the trash.
func (w WorkWriter) RemoveTask(ctx context.Context, opID, id, project string, subtree bool,
	notify *tracker.Notify) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opRemoveTask, &w.actor, removeTaskArgs{
		OpID: opID, ID: id, Project: project, Subtree: subtree, Notify: notify,
	})
	w.settled(out.Result)
	return out, err
}

// RestoreTask brings a task back out of the trash — alone, refused with
// [tracker.ErrRestoresOthers] when its removal took others, if alone is set.
func (w WorkWriter) RestoreTask(ctx context.Context, opID, id, project string,
	alone bool, notify *tracker.Notify) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opRestoreTask, &w.actor, restoreTaskArgs{
		OpID: opID, ID: id, Project: project, Alone: alone, Notify: notify,
	})
	w.settled(out.Result)
	return out, err
}

// PurgeTask destroys a task and everything derived from it.
func (w WorkWriter) PurgeTask(ctx context.Context, opID, id, project, reason string) (
	tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opPurgeTask, &w.actor, purgeTaskArgs{
		OpID: opID, ID: id, Project: project, Reason: reason,
	})
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

// Containers lists every container.
func (p Pages) Containers(ctx context.Context, fresh statelog.Freshness) (
	[]pages.ContainerListing, error) {
	return call(ctx, p.r, opContainers, nil, containersArgs{Fresh: fresh})
}

// Activity answers what happened to a page, a container or the whole
// knowledge base.
func (p Pages) Activity(ctx context.Context, q pages.PageActivityQuery) (pages.PageActivity, error) {
	return call(ctx, p.r, opPageActivity, nil, q)
}

// LinkedFrom answers which pages and tasks link to a page, by its id — from a
// data node whose copies of both the knowledge base and the tracker serve —
// or [search.ErrIndexBuilding] from one whose index has not finished its
// first lap.
func (p Pages) LinkedFrom(ctx context.Context, pageID string) (search.Backlinks, error) {
	return call(ctx, p.r, opLinkedFrom, nil, linkedFromArgs{PageID: pageID})
}

// Revision answers one revision of a page's body, and false where the page
// has none at that version.
func (p Pages) Revision(ctx context.Context, pageID string, version int,
	fresh statelog.Freshness) (pages.Revision, bool, error) {
	out, err := call(ctx, p.r, opRevision, nil, revisionArgs{PageID: pageID, Version: version, Fresh: fresh})
	return out.Revision, out.Found, err
}

// Create writes a new page. Never repeated when unanswered — see
// [ErrOutcomeUnknown] — unless its actor carries the caller's operation key,
// under which a repeat is the same operation ([op.repeatableWhen]). So for
// every page write below; a comment may carry its turn's key instead.
func (p Pages) Create(ctx context.Context, actor pages.Actor, in pages.NewPage) (pages.Written, error) {
	out, err := call(ctx, p.r, opCreatePage, nil, createPageArgs{Actor: actor, Page: in})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// SavePage saves a page's body.
func (p Pages) SavePage(ctx context.Context, actor pages.Actor, pageID string,
	save pages.Save) (pages.Written, error) {
	out, err := call(ctx, p.r, opSavePage, nil, savePageArgs{Actor: actor, PageID: pageID, Save: save})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// Rename moves a page to a new title.
func (p Pages) Rename(ctx context.Context, actor pages.Actor, pageID, title string,
	quiet bool) (pages.Written, error) {
	out, err := call(ctx, p.r, opRenamePage, nil, renamePageArgs{
		Actor: actor, PageID: pageID, Title: title, Quiet: quiet,
	})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// Comment adds a comment.
func (p Pages) Comment(ctx context.Context, actor pages.Actor, pageID string,
	in pages.NewComment) (pages.Comment, pages.Written, error) {
	out, err := call(ctx, p.r, opCommentPage, nil, commentArgs{Actor: actor, PageID: pageID, Comment: in})
	p.r.Observe(out.Written.Outcome.Position)
	return out.Comment, out.Written, err
}

// EditComment rewrites a comment.
func (p Pages) EditComment(ctx context.Context, actor pages.Actor, pageID, commentID,
	body string) (pages.Comment, pages.Written, error) {
	out, err := call(ctx, p.r, opEditComment, nil, editCommentArgs{
		Actor: actor, PageID: pageID, CommentID: commentID, Body: body,
	})
	p.r.Observe(out.Written.Outcome.Position)
	return out.Comment, out.Written, err
}

// Trash moves a page to the trash.
func (p Pages) Trash(ctx context.Context, actor pages.Actor, pageID string) (pages.Written, error) {
	out, err := call(ctx, p.r, opTrashPage, nil, pageRefArgs{Actor: actor, PageID: pageID})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// Restore brings a page back out of the trash.
func (p Pages) Restore(ctx context.Context, actor pages.Actor, pageID string) (pages.Written, error) {
	out, err := call(ctx, p.r, opRestorePage, nil, pageRefArgs{Actor: actor, PageID: pageID})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// Purge destroys a page and everything derived from it.
func (p Pages) Purge(ctx context.Context, actor pages.Actor, pageID, reason string) (
	pages.Written, error) {
	out, err := call(ctx, p.r, opPurgePage, nil, purgePageArgs{
		Actor: actor, PageID: pageID, Reason: reason,
	})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// RemoveComment takes one remark down, under the authority the caller's table
// decided.
func (p Pages) RemoveComment(ctx context.Context, actor pages.Actor, pageID, commentID string,
	authority pages.CommentAuthority) (pages.Written, error) {
	out, err := call(ctx, p.r, opRemoveComment, nil, removeCommentArgs{
		Actor: actor, PageID: pageID, CommentID: commentID, Authority: authority,
	})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// Identity is the identity directory's read side, answered by a data node:
// what a node holding no data asks of it — see [IdentityReader].
type Identity struct{ r *Router }

// Identity answers the identity directory's reads.
func (r *Router) Identity() Identity { return Identity{r: r} }

// BindingByLogin answers the directory row a login binds, narrowed to the
// binding ([Binding]), and how far the node that answered could vouch for it.
// A login nobody holds is the zero binding and no error.
//
// THE VOUCH IS THE ANSWERING NODE'S, which is the node whose rows the binding
// was read from: a lag or a retained record there is what the caller's own
// table weighs, exactly as it weighs this node's where it reads its own.
func (i Identity) BindingByLogin(ctx context.Context, login string) (Binding,
	iamdomain.Vouch, error) {
	out, err := call(ctx, i.r, opIdentityBinding, nil, bindingArgs{Login: login})
	return out.Binding, out.Vouch, err
}

// SeatHolders answers every seat binding the directory holds, read in one
// snapshot on the node that answered. An error is UNKNOWN, never an empty
// directory.
func (i Identity) SeatHolders(ctx context.Context) ([]iamdomain.SeatHolder, error) {
	return call(ctx, i.r, opSeatHolders, nil, struct{}{})
}

// MachineToken answers the row a presented machine token is checked against —
// [credential.CheckToken]'s input — as a data node holds it, for a node that
// holds no identity rows of its own.
//
// THE PRESENTED TOKEN STAYS HERE. What is asked is its id, a nonce drawn for
// this question alone and the keyring entry to MAC under; what comes back is
// the row with its verifier withheld and a MAC over it ([TokenFacts]); and the
// verifier set on the answer is the one presented implies, only where that MAC
// confirms it is the stored one. So the table decides here exactly what it
// decides on a data node, and neither the secret nor the verifier crosses. A
// token named by its id alone — a view ticket's, whose secret the request that
// minted it proved — is answered with no verifier, which [credential.CheckHeld]
// never reads.
//
// THE VOUCH IS THE ANSWERING NODE'S, as a binding's is
// ([Identity.BindingByLogin]): the applied position an absent row is weighed
// against, and the lag and the deferral the coverage is judged on, are the
// rows'.
//
// AN ERROR IS THE UNKNOWN ARM, always: a row no data node could read, no data
// node answering, and this node holding no key to check an answer under are
// all "cannot say", never "no such token".
func (i Identity) MachineToken(ctx context.Context, presented credential.Token) (
	credential.TokenRow, error) {

	keyID, key, held := i.r.env.verifiers.asking()
	if !held {
		return credential.TokenRow{}, fmt.Errorf("estate: this node holds no key to "+
			"check a machine token's answer under: secrets.active_key_id %q names "+
			"none of secrets.keys", keyID)
	}
	// rand.Read never returns an error: it crashes the process rather than
	// hand back a nonce it could not fill.
	var nonce [nonceBytes]byte
	_, _ = rand.Read(nonce[:])
	facts, err := call(ctx, i.r, opIdentityMachineToken, nil, machineTokenArgs{
		ID: presented.ID, Nonce: nonce, KeyID: keyID,
	})
	if err != nil {
		return credential.TokenRow{}, err
	}
	return facts.rowFor(presented, key, nonce), nil
}

// Knowledge is the native knowledge search, answered by a data node's index.
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
// answers no hits, because a turn must not die because the node holding the
// index — this one or another — was slow.
//
// AND IT SAYS IT DID NOT RUN, which is not "nothing matched": a search no data
// node could answer serves NO mode and covers NONE of the corpus's buckets, so
// every reader of the outcome — the search tool, the turn-start block, a screen
// — tells the seat the knowledge base could not be searched rather than that
// the company has written nothing down, which it would act on by writing a
// duplicate of a page that exists.
func (k Knowledge) Search(ctx context.Context, q knowledge.Query) knowledge.Result {
	args := knowledgeArgs{Text: q.Text, Limit: q.Limit, Mode: q.Mode, Scoped: q.Org != nil,
		Prefetch: q.Prefetch}
	if q.Seat != nil {
		args.Seat = q.Seat.Handle()
	}
	if q.ExcludeAncestors != nil {
		args.Exclusion, args.ExcludeAncestors = true, q.ExcludeAncestors
	}
	result, err := call(ctx, k.r, opKnowledgeSearch, nil, args)
	if err != nil {
		log.WarnContext(ctx, "knowledge_search_failed", "error", err.Error(),
			"detail", "the knowledge block says the search did not run; a turn must "+
				"not die because the node holding the index was slow")
		return notSearched()
	}
	return result
}

// notSearched is the answer to a knowledge search no data node ran: no hits,
// no mode served, and every bucket of the corpus missing — the outcome every
// reader already renders as "could not be searched".
func notSearched() knowledge.Result {
	return knowledge.Result{Outcome: knowledge.Outcome{
		Coverage: knowledge.Coverage{
			Nodes: []knowledge.NodeCoverage{}, BucketsMissing: search.SearchShards,
		},
	}}
}

// Building reports whether the index a search reads is still building, which
// is what turns an empty block into "still indexing" rather than "the company
// has written nothing down". False when nothing answers.
func (k Knowledge) Building(ctx context.Context) bool {
	building, err := call(ctx, k.r, opKnowledgeBuilding, nil, struct{}{})
	return err == nil && building
}

// Usage is the usage domain's reads (ADR-0020), answered by a data node whose
// usage copy is in service — this node's own where it is one (ADR-0049).
type Usage struct{ r *Router }

// Usage answers the usage domain.
func (r *Router) Usage() Usage { return Usage{r: r} }

// Spend answers every node's spend cells over a range of company days.
func (u Usage) Spend(ctx context.Context, q usage.SpendQuery) ([]usage.SpendRow, error) {
	return allPages(ctx, func(ctx context.Context, after usage.Group) (usage.Page[usage.SpendRow], error) {
		return call(ctx, u.r, opUsageSpend, nil, usagePageArgs[usage.SpendQuery]{Query: q, After: after})
	})
}

// SeatTurns answers every node's turn counts over a range of company days.
func (u Usage) SeatTurns(ctx context.Context, q usage.SpendQuery) ([]usage.TurnsRow, error) {
	return allPages(ctx, func(ctx context.Context, after usage.Group) (usage.Page[usage.TurnsRow], error) {
		return call(ctx, u.r, opUsageSeatTurns, nil, usagePageArgs[usage.SpendQuery]{Query: q, After: after})
	})
}

// PersonSpend answers every node's spend for people over a range of company
// days.
func (u Usage) PersonSpend(ctx context.Context, q usage.PersonQuery) ([]usage.PersonRow, error) {
	return allPages(ctx, func(ctx context.Context, after usage.Group) (usage.Page[usage.PersonRow], error) {
		return call(ctx, u.r, opUsagePersonSpend, nil, usagePageArgs[usage.PersonQuery]{Query: q, After: after})
	})
}

// SeatDays answers every node's head rows over a range of company days.
func (u Usage) SeatDays(ctx context.Context, q usage.SpendQuery) ([]usage.SeatDay, error) {
	return allPages(ctx, func(ctx context.Context, after usage.Group) (usage.Page[usage.SeatDay], error) {
		return call(ctx, u.r, opUsageSeatDays, nil, usagePageArgs[usage.SpendQuery]{Query: q, After: after})
	})
}

// ScheduleRuns answers one page of every node's schedule fires, and whether
// there are more.
func (u Usage) ScheduleRuns(ctx context.Context, q usage.ScheduleRunsQuery) (
	[]usage.ScheduleRun, bool, error) {
	page, err := call(ctx, u.r, opUsageScheduleRuns, nil, q)
	return page.Runs, page.More, err
}

// PageReads answers every node's reads of some pages over a range of company
// days.
func (u Usage) PageReads(ctx context.Context, q usage.PageReadsQuery) ([]usage.PageRead, error) {
	return allPages(ctx, func(ctx context.Context, after usage.Group) (usage.Page[usage.PageRead], error) {
		return call(ctx, u.r, opUsagePageReads, nil, usagePageArgs[usage.PageReadsQuery]{Query: q, After: after})
	})
}

// allPages reads every page of one paged read, in order — each page its own
// routed call, so a failover between pages resumes on another data node from
// the same group, since every data node pages by the one rule ([usage.Fill]).
// A page that does not end AFTER the one before it is refused: a node paging
// another way would otherwise loop for ever.
func allPages[R any](ctx context.Context,
	page func(ctx context.Context, after usage.Group) (usage.Page[R], error)) ([]R, error) {

	var out []R
	var after usage.Group
	for {
		p, err := page(ctx, after)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Rows...)
		if p.Done {
			return out, nil
		}
		if !after.Before(p.Next) {
			return nil, fmt.Errorf("estate: a usage page ended at %v, which is not after %v — "+
				"the answering node pages by another rule", p.Next, after)
		}
		after = p.Next
	}
}

// ReadsElided answers how many reads the per-seat-day cap dropped over a range
// of company days, on every node.
func (u Usage) ReadsElided(ctx context.Context, from, to string) (int64, error) {
	return call(ctx, u.r, opUsageReadsElided, nil, readsElidedArgs{From: from, To: to})
}

// RepoReadersAnswer is one page of a repository's readers, summed across every
// node and day of the range ([usage.RepoReadsPage]).
type RepoReadersAnswer struct {
	// Readers are the page's readers, in reader order.
	Readers []usage.RepoReader

	// Elided is how many readers the per-day cap dropped over the range on
	// every node — on the FIRST page alone, the one a reader starts from; nil
	// on every page after it, which a cursor already ties to that answer.
	Elided *int64

	// Next is the cursor of the page after this one, "" on the last.
	Next string
}

// RepoReaders answers one page of a repository's readers over a range of
// company days, cut at pageBytes and resuming at cursor — "" for the first
// page, and otherwise a page's own [RepoReadersAnswer.Next] — and with the
// first page how many readers the per-day cap dropped over the range.
//
// A PAGE A ROUTED CALL: each is asked of whichever data node serves the usage
// copy now, and every data node cuts and resumes by the one rule
// ([usage.Fill]), so a reader walked across a failover reads every reader once.
// A cursor or a range the read cannot take is refused naming why
// ([usage.ErrBadCursor], [usage.ErrBadRange]).
func (u Usage) RepoReaders(ctx context.Context, q usage.RepoReadsQuery, cursor string,
	pageBytes int) (RepoReadersAnswer, error) {

	if err := q.Validate(); err != nil {
		return RepoReadersAnswer{}, err
	}
	after, err := usage.ParseCursor(cursor)
	if err != nil {
		return RepoReadersAnswer{}, err
	}
	page, err := call(ctx, u.r, opUsageRepoReads, nil, repoReadsArgs{Query: q, After: after, Budget: pageBytes})
	if err != nil {
		return RepoReadersAnswer{}, err
	}
	out := RepoReadersAnswer{Readers: page.Rows}
	if !page.Done {
		if !after.Before(page.Next) {
			return RepoReadersAnswer{}, fmt.Errorf("estate: a page of readers ended at %v, which is not "+
				"after %v — the answering node pages by another rule", page.Next, after)
		}
		out.Next = page.Next.Cursor()
	}
	if cursor == "" {
		elided, err := call(ctx, u.r, opUsageRepoReadsElided, nil, q)
		if err != nil {
			return RepoReadersAnswer{}, err
		}
		out.Elided = &elided
	}
	return out, nil
}

// CodeSeam is the code host's reach into the estate, from every node: a push's
// proposal, a compaction's, an expunge record's and a change's to their
// repository's committer, a write of a change's conversation to the change
// writer beside it and a write of a repository's checks to the checks writer
// beside it, and the reads the host's admission, a cache's sync and the
// REST routes make — each answered by a data node whose copy of the code log
// serves, this one where it does — and a repository's read by object id and its
// engine write, each asked of the repository's home git node ([GitCopy]). Its
// consumers declare what they call of it.
//
// ONLY THIS NODE'S OWN WRITES RAISE ITS FLOOR: a proposal that landed — a
// push's, a compaction's, an expunge record's or a change's — a push this
// node proposed found landed, a write of a change's conversation or of a
// repository's checks that landed, and an engine write this node asked that
// landed. A read's position is
// somebody else's writes, and observing it would hold every later read this
// node routes — a `stale` one included — behind the newest linearizable read
// anybody on the node made, which is the tracker's and the knowledge base's
// rule too. Each such write raises two floors: the session's, on the whole
// code log, which every operation this node routes to a data node carries;
// and the floor of the REPOSITORY it wrote ([CodeSeam.FloorOf]), which a read
// of that repository's content by object id carries to its git node — and a
// read of any other repository does not. A conversation's write and a check's
// move no git content, and raise the session's alone.
type CodeSeam struct{ r *Router }

// Code answers the code host.
func (r *Router) Code() CodeSeam { return CodeSeam{r: r} }

// Propose asks the repository's committer to decide a push, and raises this
// node's floors past the record it landed in — this node's own write, which
// its next read of the repository must see on whichever node answers.
func (c CodeSeam) Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error) {
	out, err := call(ctx, c.r, opCodePropose, nil, p)
	if out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending {
		c.wrote(p.RepoID, out.Position)
	}
	return out, err
}

// Compact asks the repository's committer to decide a compaction, and raises
// this node's floors past the record it landed in — this node's own write,
// which the compactor's next listing and the cache's sync after it must see on
// whichever data node answers them.
func (c CodeSeam) Compact(ctx context.Context, p code.CompactionProposal) (code.CompactResult, error) {
	out, err := call(ctx, c.r, opCodeCompact, nil, p)
	if out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending {
		c.wrote(p.Compaction.RepoID, out.Position)
	}
	return out, err
}

// Expunge asks the repository's committer to decide an expunge record — a
// person's purge request, or a maintainer's begin or commit — and raises this
// node's floors past the record it landed in — this node's own write, which
// its next read of the repository must see on whichever node answers it.
func (c CodeSeam) Expunge(ctx context.Context, p code.ExpungeProposal) (code.ExpungeResult, error) {
	out, err := call(ctx, c.r, opCodeExpunge, nil, p)
	if out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending {
		c.wrote(p.RepoID(), out.Position)
	}
	return out, err
}

// Change asks the repository's committer to decide a write of a change's
// lifecycle, and raises this node's floors past the record it landed in — this
// node's own write, which its next read of the change must see on whichever
// node answers it. A write answered UNCHANGED landed no record, and raises
// nothing: the position it names is the change's last record, somebody else's
// write.
func (c CodeSeam) Change(ctx context.Context, p code.ChangeProposal) (code.ChangeResult, error) {
	out, err := call(ctx, c.r, opCodeChange, nil, p)
	if !out.Unchanged && (out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending) {
		c.wrote(p.RepoID(), out.Position)
	}
	return out, err
}

// Queue asks the repository's committer to decide a write of a target's merge
// queue, and raises this node's floors past the record it landed in — this
// node's own write, which its next read of the change and the queue must see
// on whichever node answers it.
func (c CodeSeam) Queue(ctx context.Context, p code.QueueProposal) (code.QueueResult, error) {
	out, err := call(ctx, c.r, opCodeQueue, nil, p)
	if out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending {
		c.wrote(p.RepoID(), out.Position)
	}
	return out, err
}

// Merge asks the repository's committer to decide a change's merge, and raises
// this node's floors past the record it landed in — this node's own write,
// which its next read of the change, its branches and its history must see on
// whichever node answers it.
func (c CodeSeam) Merge(ctx context.Context, p code.MergeProposal) (code.MergeResult, error) {
	out, err := call(ctx, c.r, opCodeMerge, nil, p)
	if out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending {
		c.wrote(p.RepoID(), out.Position)
	}
	return out, err
}

// Converse asks the repository's committing data node to decide a write of a
// change's conversation, and raises this node's SESSION floor past the record
// it landed in — this node's own write, which its next read of the change
// must see on whichever node answers it. NEVER THE REPOSITORY'S OWN FLOOR,
// which a read of the repository's git objects carries as its git node's sync
// level: a conversation moves no ref and no pack, and raising it would make
// that node sync for nothing on the repository's next read. A write answered
// UNCHANGED landed no record, and raises nothing.
func (c CodeSeam) Converse(ctx context.Context, w code.ConversationWrite) (code.ConversationResult, error) {
	out, err := call(ctx, c.r, opCodeConversation, nil, w)
	if !out.Unchanged && (out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending) {
		c.r.Observe(out.Position)
	}
	return out, err
}

// Checks asks the repository's committing data node to decide a write of the
// repository's checks, and raises this node's SESSION floor past the record it
// landed in — this node's own write, which its next read of the attempt must
// see on whichever node answers it. NEVER THE REPOSITORY'S OWN FLOOR, for a
// conversation's reason: a check moves no ref and no pack. A write answered
// UNCHANGED landed no record, and raises nothing.
func (c CodeSeam) Checks(ctx context.Context, r code.ChecksRequest) (code.ChecksResult, error) {
	out, err := call(ctx, c.r, opCodeChecks, nil, r)
	if !out.Unchanged && (out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending) {
		c.r.Observe(out.Position)
	}
	return out, err
}

// wrote raises this node's floors past at, where its own write to repository
// repoID landed: the session's on the code log, and the repository's own.
func (c CodeSeam) wrote(repoID string, at statelog.Position) {
	c.r.Observe(at)
	c.r.codeWrites.observe(repoID, at)
}

// Resolve answers a repository by its name.
func (c CodeSeam) Resolve(ctx context.Context, q code.ResolveQuery) (code.RepoRef, error) {
	return call(ctx, c.r, opCodeResolve, nil, q)
}

// RepoState answers one page of a repository's snapshot.
func (c CodeSeam) RepoState(ctx context.Context, q code.StateQuery) (code.RepoSnapshot, error) {
	return call(ctx, c.r, opCodeRepoState, nil, q)
}

// PushOutcome answers whether a push this node proposed landed — and, where it
// did, raises this node's floors past it, for [CodeSeam.Propose]'s reason.
func (c CodeSeam) PushOutcome(ctx context.Context, q code.OutcomeQuery) (code.PushOutcome, error) {
	out, err := call(ctx, c.r, opCodePushOutcome, nil, q)
	if out.Found {
		c.wrote(q.RepoID, out.Push.Position)
	}
	return out, err
}

// Read answers a read of the code host: a person's or a surface's, and the
// engine's own that no principal makes — a cache's `introduced` backfill, a
// compactor's `compaction_due` listing, a collected coding run's `run_pushes`.
func (c CodeSeam) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	return call(ctx, c.r, opCodeRead, nil, q)
}

// Lifecycle asks a data node to decide one lifecycle request — a tool's
// create, rename, settings change, archive, unarchive, delete, restore or
// protection, or a live turn's hold — and raises this node's floors past the
// record it landed in where it APPLIED or is PENDING: this node's own write,
// which its next read of the repository must see on whichever node answers.
// Never for a refusal, nor for an answer that needed no record, whose
// position is the rows somebody else wrote.
func (c CodeSeam) Lifecycle(ctx context.Context, r code.LifecycleRequest) (code.LifecycleResult, error) {
	out, err := call(ctx, c.r, opCodeLifecycle, nil, r)
	wrote := out.Refusal == "" && !out.Position.IsZero() && out.Repo.ID != "" &&
		(out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending)
	if wrote {
		c.wrote(out.Repo.ID, out.Position)
	}
	return out, err
}

// ResolveRef answers one ref of one repository at the level the query names.
// It never raises this node's floor: a read's position is somebody else's
// writes.
func (c CodeSeam) ResolveRef(ctx context.Context, q code.RefQuery) (code.RefAnswer, error) {
	return call(ctx, c.r, opCodeResolveRef, nil, q)
}

// GitRead asks the repository's home git node for one read of its content by
// object id, and its runner-up when the home cannot answer (ADR-0059). Its
// floor is the query's own (code.GitQuery.Fresh), which [CodeSeam.FloorOf]
// supplies where the reader's own writes must be in the copy it reads.
func (c CodeSeam) GitRead(ctx context.Context, q code.GitQuery) (code.GitAnswer, error) {
	return call(ctx, c.r, opCodeGitRead, nil, q)
}

// MergePreview asks the repository's home git node what merging q.Head into
// q.Base would do — by the merge the home records a change's mergeability by
// — and its runner-up when the home cannot answer (ADR-0059), as a read by
// object id is asked. Its floor is the query's own
// (code.MergePreviewQuery.Fresh), which [CodeSeam.FloorOf] supplies where the
// asker's own pushes must be in the copy it merges. It never raises this
// node's floor: a preview writes nothing.
func (c CodeSeam) MergePreview(ctx context.Context, q code.MergePreviewQuery) (code.MergePreviewAnswer, error) {
	return call(ctx, c.r, opCodeMergePreview, nil, q)
}

// GitWrite asks the repository's home git node to build one engine write and
// land it as a push (ADR-0062), and its runner-up with the same write — the
// same push — where the home ran nothing or went silent (ADR-0059). Its floor
// is the write's own (code.EngineWrite.Fresh), as a read's is.
//
// THE ROUTER THAT CARRIES A WRITE HOLDS IT TO THE ONE RULE every party to it
// does (code.EngineWrite.Validate): one it refuses is code.ErrInvalid, naming
// the field, before any git node is asked — never a round trip, a sync and a
// slot of a git node's lane for an answer every node would give alike.
//
// It raises this node's floors past the push's record where the write APPLIED
// or is PENDING — this node's own write, which its next read of the
// repository must see on whichever node answers, as a push it proposed does —
// a write answered from the row an earlier ask of it landed included, since
// that push is this one. Never for a refusal, of the change or of the push,
// which landed nothing, nor for an outcome UNKNOWN, whose record may not
// exist.
func (c CodeSeam) GitWrite(ctx context.Context, w code.EngineWrite) (code.EngineWriteResult, error) {
	if err := w.Validate(); err != nil {
		return code.EngineWriteResult{}, err
	}
	out, err := call(ctx, c.r, opCodeGitWrite, nil, w)
	if out.Refusal == "" && (out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending) {
		c.wrote(w.RepoID, out.Position)
	}
	return out, err
}

// FloorOf is this node's floor on repository repoID — the furthest of its own
// writes TO THAT REPOSITORY it has been told landed — or the zero position
// where it holds none: what a reader hands a read by object id of the
// repository as its Fresh, so the home's copy is brought to this node's own
// pushes there before it is read, since a git operation carries no floor of
// the router's ([GitCopy]).
//
// NEVER THE SESSION'S FLOOR, which is the furthest of this node's writes
// anywhere on the code log: a push to one repository would floor every later
// read by object id of every other, whose home's copy — current to every push
// of ITS repository — would sit below that floor and run a sync round for
// nothing, on every read, for as long as this node goes on writing anywhere.
func (c CodeSeam) FloorOf(repoID string) statelog.Position {
	return c.r.codeWrites.floor(repoID)
}
