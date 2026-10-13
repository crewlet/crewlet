package estate

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/code"
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
// does (ADR-0031).
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
		Watch: patch.Watch, Relate: patch.Relate, Depend: patch.Depend, Promote: patch.Promote,
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

// WriteView saves a view.
func (w WorkWriter) WriteView(ctx context.Context, opID string, view tracker.View) (
	tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opWriteView, &w.actor, writeViewArgs{OpID: opID, View: view})
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

// MarkInbox applies one gesture to a person's inbox.
func (w WorkWriter) MarkInbox(ctx context.Context, opID, handle string,
	gesture tracker.InboxGesture) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opMarkInbox, &w.actor,
		markInboxArgs{OpID: opID, Handle: handle, Gesture: gesture})
	w.settled(out.Result)
	return out, err
}

// WritePins applies one gesture to a person's pinned views and favourites.
func (w WorkWriter) WritePins(ctx context.Context, opID, handle string,
	gesture tracker.PinGesture) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opWritePins, &w.actor,
		writePinsArgs{OpID: opID, Handle: handle, Gesture: gesture})
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

// RestoreTask brings a task back out of the trash.
func (w WorkWriter) RestoreTask(ctx context.Context, opID, id, project string,
	notify *tracker.Notify) (tracker.WriteResult, error) {
	out, err := call(ctx, w.r, opRestoreTask, &w.actor, restoreTaskArgs{
		OpID: opID, ID: id, Project: project, Notify: notify,
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
// [ErrOutcomeUnknown] — unless it carries the caller's key, under which a
// repeat is the same operation ([op.repeatableWhen]). So for every page write
// below.
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
	quiet bool, key pages.CallKey) (pages.Written, error) {
	out, err := call(ctx, p.r, opRenamePage, nil, renamePageArgs{
		Actor: actor, PageID: pageID, Title: title, Quiet: quiet, Key: key,
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
	body string, key pages.CallKey) (pages.Comment, pages.Written, error) {
	out, err := call(ctx, p.r, opEditComment, nil, editCommentArgs{
		Actor: actor, PageID: pageID, CommentID: commentID, Body: body, Key: key,
	})
	p.r.Observe(out.Written.Outcome.Position)
	return out.Comment, out.Written, err
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
	args := knowledgeArgs{Text: q.Text, Limit: q.Limit, Mode: q.Mode, Scoped: q.Org != nil}
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
// usage copy is in service — this node's own where it is one (ADR-0031).
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

// CodeSeam is the code host's reach into the estate, from every node: a push's
// proposal to its repository's committer, and the reads the host's admission,
// a cache's sync and the REST routes make — each answered by a data node whose
// copy of the code log serves, this one where it does. Its consumers declare
// what they call of it.
//
// ONLY THIS NODE'S OWN WRITES RAISE ITS FLOOR: a proposal that landed, and a
// push this node proposed found landed. A read's position is somebody else's
// writes, and observing it would hold every later read this node routes — a
// `stale` one included — behind the newest linearizable read anybody on the
// node made, which is the tracker's and the knowledge base's rule too.
type CodeSeam struct{ r *Router }

// Code answers the code host.
func (r *Router) Code() CodeSeam { return CodeSeam{r: r} }

// Propose asks the repository's committer to decide a push, and raises this
// node's session floor past the record it landed in — this node's own write,
// which its next read of the repository must see on whichever node answers.
func (c CodeSeam) Propose(ctx context.Context, p code.Proposal) (code.ProposeResult, error) {
	out, err := call(ctx, c.r, opCodePropose, nil, p)
	if out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending {
		c.r.Observe(out.Position)
	}
	return out, err
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
// did, raises this node's floor past it, for [CodeSeam.Propose]'s reason.
func (c CodeSeam) PushOutcome(ctx context.Context, q code.OutcomeQuery) (code.PushOutcome, error) {
	out, err := call(ctx, c.r, opCodePushOutcome, nil, q)
	if out.Found {
		c.r.Observe(out.Push.Position)
	}
	return out, err
}

// Read answers a person's or a surface's read of the code host.
func (c CodeSeam) Read(ctx context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	return call(ctx, c.r, opCodeRead, nil, q)
}
