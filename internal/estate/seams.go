package estate

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The asking half: the seams every node's tools are handed, each answered by a
// data node whose copy of the estate serves — this one where it does.
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
func (w Work) Tasks(ctx context.Context, q worklet.Query, now time.Time) (worklet.Answer, error) {
	return call(ctx, w.r, opTasks, nil, tasksArgs{Query: q, Now: now})
}

// Task answers one task, whole.
func (w Work) Task(ctx context.Context, idOrKey string, want worklet.DetailWants,
	fresh statelog.Freshness) (worklet.TaskDetail, error) {
	args := taskArgs{IDOrKey: idOrKey, Want: want, Fresh: fresh}
	if want.Clock != nil {
		args.Zone = zoneArg(want.Clock.Zone)
	}
	return call(ctx, w.r, opTask, nil, args)
}

// Files answers a page of a project's files.
func (w Work) Files(ctx context.Context, q worklet.FileQuery) (worklet.FileListing, error) {
	return call(ctx, w.r, opFiles, nil, q)
}

// File answers one file with the object holding its bytes.
func (w Work) File(ctx context.Context, project, path string,
	fresh statelog.Freshness) (worklet.FileDetail, error) {
	return call(ctx, w.r, opFile, nil, fileArgs{Project: project, Path: path, Fresh: fresh})
}

// Views answers the saved views.
func (w Work) Views(ctx context.Context, q worklet.ViewQuery) (worklet.ViewListing, error) {
	return call(ctx, w.r, opViews, nil, viewsArgs{Query: q, Zone: zoneArg(q.Zone)})
}

// ExpandedQuery resolves a saved view's parameters into a query.
func (w Work) ExpandedQuery(ctx context.Context, params map[string]any,
	viewer worklet.Viewer, now time.Time, loc *time.Location) (worklet.Query, error) {
	return call(ctx, w.r, opExpandedQuery, nil,
		expandArgs{Params: params, Viewer: viewer, Now: now, Zone: zoneArg(loc)})
}

// Catalogue answers the workspace catalogue.
func (w Work) Catalogue(ctx context.Context, q worklet.CatalogueQuery) (worklet.CatalogueAnswer, error) {
	return call(ctx, w.r, opCatalogue, nil, q)
}

// Person answers one person's own record.
func (w Work) Person(ctx context.Context, q worklet.PersonQuery, now time.Time) (worklet.PersonState, error) {
	return call(ctx, w.r, opPerson, nil, personArgs{Query: q, Now: now})
}

// Thread answers one ask-and-answer thread on a task.
func (w Work) Thread(ctx context.Context, q worklet.ThreadQuery,
	fresh statelog.Freshness) (worklet.ResolvedThread, error) {
	return call(ctx, w.r, opThread, nil, threadArgs{Query: q, Fresh: fresh})
}

// Activity answers what happened to a task or a project.
func (w Work) Activity(ctx context.Context, q worklet.ActivityQuery, now time.Time) (
	worklet.ActivityAnswer, error) {
	return call(ctx, w.r, opActivity, nil, activityArgs{Query: q, Now: now})
}

// MyWork answers what is on one seat's plate, "today" cut on loc.
func (w Work) MyWork(ctx context.Context, q worklet.MyWorkQuery, now time.Time,
	loc *time.Location) (worklet.MyWork, error) {
	return call(ctx, w.r, opMyWork, nil, myWorkArgs{Query: q, Now: now, Zone: zoneArg(loc)})
}

// Projects lists the company's projects.
func (w Work) Projects(ctx context.Context, q worklet.ProjectQuery) (worklet.ProjectListing, error) {
	return call(ctx, w.r, opProjects, nil, q)
}

// Project describes one project.
func (w Work) Project(ctx context.Context, q worklet.ProjectDetailQuery) (worklet.ProjectDetail, error) {
	return call(ctx, w.r, opProject, nil, q)
}

// Search is the tracker's ranked search, answered by a data node's index. A
// search no node could run is its error, never an empty answer: "nothing
// matched" would send the seat off to file what it was looking for.
func (w Work) Search(ctx context.Context, q worklet.SearchQuery) (worklet.SearchAnswer, error) {
	if !q.Mode.Valid() {
		return worklet.SearchAnswer{}, fmt.Errorf("worklet: unknown search mode %q; "+
			"the modes are hybrid, keyword and semantic", q.Mode)
	}
	return call(ctx, w.r, opWorkSearch, nil, workSearchArgs{Text: q.Text, Limit: q.Limit, Mode: q.Mode})
}

// Workload answers a unit's workload, its days cut on loc.
func (w Work) Workload(ctx context.Context, q worklet.WorkloadQuery, now time.Time,
	loc *time.Location) (worklet.WorkloadAnswer, error) {
	return call(ctx, w.r, opWorkload, nil, workloadArgs{Query: q, Now: now, Zone: zoneArg(loc)})
}

// Inbox answers what the company has asked of one person.
func (w Work) Inbox(ctx context.Context, q worklet.InboxQuery, now time.Time) (
	worklet.InboxAnswer, error) {
	return call(ctx, w.r, opInbox, nil, inboxArgs{Query: q, Now: now})
}

// Routing answers who a change reached, and why.
func (w Work) Routing(ctx context.Context, q worklet.RoutingQuery, now time.Time) (
	worklet.RoutingAnswer, error) {
	return call(ctx, w.r, opRouting, nil, routingArgs{Query: q, Now: now})
}

// TurnsOf answers a page of the turns charged to one task.
func (w Work) TurnsOf(ctx context.Context, idOrKey, cursor string, limit int,
	fresh statelog.Freshness) (worklet.TaskTurns, error) {
	return call(ctx, w.r, opTurnsOf, nil,
		turnsOfArgs{IDOrKey: idOrKey, Cursor: cursor, Limit: limit, Fresh: fresh})
}

// EveryView answers every saved view, whoever owns it.
func (w Work) EveryView(ctx context.Context, q worklet.EveryViewQuery) (worklet.ViewListing, error) {
	return call(ctx, w.r, opEveryView, nil, everyViewArgs{Query: q, Zone: zoneArg(q.Zone)})
}

// Flow answers how work moved through the company, its days cut on loc.
func (w Work) Flow(ctx context.Context, q worklet.FlowQuery, now time.Time,
	loc *time.Location) (worklet.FlowAnswer, error) {
	return call(ctx, w.r, opFlow, nil, flowArgs{Query: q, Now: now, Zone: zoneArg(loc)})
}

// CompanyFeed answers a page of the company's feed.
func (w Work) CompanyFeed(ctx context.Context, q worklet.FeedQuery) (worklet.FeedPage, error) {
	return call(ctx, w.r, opCompanyFeed, nil, q)
}

// Decisions answers the structured asks waiting on a decision, "overdue" cut
// on loc.
func (w Work) Decisions(ctx context.Context, q worklet.DecisionsQuery, now time.Time,
	loc *time.Location) (worklet.DecisionsAnswer, error) {
	return call(ctx, w.r, opDecisions, nil, decisionsArgs{Query: q, Now: now, Zone: zoneArg(loc)})
}

// AnsweredAsks answers the asks put to a person and closed lately.
func (w Work) AnsweredAsks(ctx context.Context, q worklet.DecisionsQuery, now time.Time,
	loc *time.Location) (worklet.AnsweredAnswer, error) {
	return call(ctx, w.r, opAnsweredAsks, nil, decisionsArgs{Query: q, Now: now, Zone: zoneArg(loc)})
}

// TurnPlaces answers which task each run was charged to.
func (w Work) TurnPlaces(ctx context.Context, runs []string,
	fresh statelog.Freshness) (map[string]worklet.TurnPlace, error) {
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
func (w WorkWriter) CreateTask(ctx context.Context, opID string, task worklet.Task,
	notify *worklet.Notify) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opCreateTask, &w.actor,
		createTaskArgs{OpID: opID, Task: task, Notify: notify})
	w.settled(out.Result)
	return out, err
}

// CreateTaskAsking files a task with a structured ask on it, in one gesture.
func (w WorkWriter) CreateTaskAsking(ctx context.Context, opID string, task worklet.Task,
	ask worklet.Comment, notify *worklet.Notify) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opCreateTaskAsking, &w.actor,
		createTaskAskingArgs{OpID: opID, Task: task, Ask: ask, Notify: notify})
	w.settled(out.Result)
	return out, err
}

// PlaceTask drops a card on the board: its lane, then its place.
func (w WorkWriter) PlaceTask(ctx context.Context, opID string, place worklet.Place,
	notify *worklet.Notify) (worklet.PlaceResult, error) {
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
	patch worklet.TaskPatch, kind worklet.ChangeKind,
	notify *worklet.Notify) (worklet.WriteResult, error) {
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
func (w WorkWriter) Depend(ctx context.Context, opID string, change worklet.DependencyChange,
	_ worklet.Leads) (worklet.DependencyResult, error) {
	out, err := call(ctx, w.r, opDepend, &w.actor, dependArgs{OpID: opID, Change: change})
	w.settled(out.Result)
	return out, err
}

// MergeDuplicates folds one task into another.
func (w WorkWriter) MergeDuplicates(ctx context.Context, opID, duplicate, into string,
	reparent bool, notify *worklet.Notify) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opMergeDuplicates, &w.actor, mergeArgs{
		OpID: opID, Duplicate: duplicate, Into: into, Reparent: reparent, Notify: notify,
	})
	w.settled(out.Result)
	return out, err
}

// MoveTaskToProject moves a task and its subtree to another project.
func (w WorkWriter) MoveTaskToProject(ctx context.Context, opID, taskID, target string,
	notify *worklet.Notify) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opMoveTaskToProject, &w.actor, moveArgs{
		OpID: opID, TaskID: taskID, Target: target, Notify: notify,
	})
	w.settled(out.Result)
	return out, err
}

// WriteProject edits a project's own settings.
func (w WorkWriter) WriteProject(ctx context.Context, opID, key string, edit worklet.ProjectEdit,
	authority worklet.ProjectAuthority) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opWriteProject, &w.actor, writeProjectArgs{
		OpID: opID, Key: key, Edit: edit, Authority: authority,
	})
	w.settled(out.Result)
	return out, err
}

// WriteTags edits a project's declared tags.
func (w WorkWriter) WriteTags(ctx context.Context, opID, project string, edit worklet.TagEdit,
	authority worklet.TagAuthority) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opWriteTags, &w.actor, writeTagsArgs{
		OpID: opID, Project: project, Edit: edit, Authority: authority,
	})
	w.settled(out.Result)
	return out, err
}

// EnsureTags declares the tags a write is about to use.
func (w WorkWriter) EnsureTags(ctx context.Context, opID, project string, tags []string) (
	[]string, []caveat.Caveat, error) {
	out, err := call(ctx, w.r, opEnsureTags, &w.actor, ensureTagsArgs{
		OpID: opID, Project: project, Tags: tags,
	})
	return out.Created, out.Caveats, err
}

// PutFile writes a project's file — its row, naming the object the caller has
// already uploaded through its own object client.
func (w WorkWriter) PutFile(ctx context.Context, opID string, put worklet.FilePut) (
	worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opPutFile, &w.actor, putFileArgs{OpID: opID, Put: put})
	w.settled(out.Result)
	return out, err
}

// RemoveFile takes a file out of its project.
func (w WorkWriter) RemoveFile(ctx context.Context, opID, project, path string,
	ifMatch uint64) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opRemoveFile, &w.actor, removeFileArgs{
		OpID: opID, Project: project, Path: path, IfMatch: ifMatch,
	})
	w.settled(out.Result)
	return out, err
}

// RecordTurn records one turn's spend on the task it worked on.
func (w WorkWriter) RecordTurn(ctx context.Context, opID string,
	turn worklet.TurnRecord) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opRecordTurn, &w.actor, recordTurnArgs{OpID: opID, Turn: turn})
	w.settled(out.Result)
	return out, err
}

// ViewPrior reads what a save of view id would replace, for the caller to
// decide its authority on and hand back to [WorkWriter.WriteView].
func (w WorkWriter) ViewPrior(ctx context.Context, id string) (worklet.ViewPrior, error) {
	return call(ctx, w.r, opViewPrior, &w.actor, viewPriorArgs{ID: id})
}

// WriteView saves a view, refused where the stored view is no longer prior.
func (w WorkWriter) WriteView(ctx context.Context, opID string, view worklet.View,
	prior worklet.ViewPrior) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opWriteView, &w.actor,
		writeViewArgs{OpID: opID, View: view, Prior: prior})
	w.settled(out.Result)
	return out, err
}

// WriteTypes sets the company's task types.
func (w WorkWriter) WriteTypes(ctx context.Context, opID string, types []worklet.TaskType) (
	worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opWriteTypes, &w.actor, writeTypesArgs{OpID: opID, Types: types})
	w.settled(out.Result)
	return out, err
}

// WriteFields sets the company's custom fields.
func (w WorkWriter) WriteFields(ctx context.Context, opID string, fields []worklet.FieldDef) (
	worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opWriteFields, &w.actor, writeFieldsArgs{OpID: opID, Fields: fields})
	w.settled(out.Result)
	return out, err
}

// MarkInbox applies one gesture to a person's inbox, under the authority the
// caller's table decided.
func (w WorkWriter) MarkInbox(ctx context.Context, opID, handle string,
	gesture worklet.InboxGesture, authority worklet.PersonAuthority) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opMarkInbox, &w.actor, markInboxArgs{
		OpID: opID, Handle: handle, Gesture: gesture, Authority: authority,
	})
	w.settled(out.Result)
	return out, err
}

// WritePins applies one gesture to a person's pinned views and favourites,
// under the authority the caller's table decided.
func (w WorkWriter) WritePins(ctx context.Context, opID, handle string,
	gesture worklet.PinGesture, authority worklet.PersonAuthority) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opWritePins, &w.actor, writePinsArgs{
		OpID: opID, Handle: handle, Gesture: gesture, Authority: authority,
	})
	w.settled(out.Result)
	return out, err
}

// EditComment rewrites the writer's own remark on a work item.
func (w WorkWriter) EditComment(ctx context.Context, opID, taskID, project, commentID,
	body string, notify *worklet.Notify) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opEditTaskComment, &w.actor, editCommentOnTaskArgs{
		OpID: opID, TaskID: taskID, Project: project, CommentID: commentID, Body: body,
		Notify: notify,
	})
	w.settled(out.Result)
	return out, err
}

// WritePriorities sets a person's priority order, conditioned on ifMatch.
func (w WorkWriter) WritePriorities(ctx context.Context, opID, handle string,
	priorities []string, ifMatch *uint64, authority worklet.PersonAuthority) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opWritePriorities, &w.actor, writePrioritiesArgs{
		OpID: opID, Handle: handle, Priorities: priorities, IfMatch: ifMatch, Authority: authority,
	})
	w.settled(out.Result)
	return out, err
}

// RemoveTask moves a task, or its subtree, to the trash.
func (w WorkWriter) RemoveTask(ctx context.Context, opID, id, project string, subtree bool,
	notify *worklet.Notify) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opRemoveTask, &w.actor, removeTaskArgs{
		OpID: opID, ID: id, Project: project, Subtree: subtree, Notify: notify,
	})
	w.settled(out.Result)
	return out, err
}

// RestoreTask brings a task back out of the trash — alone, refused with
// [worklet.ErrRestoresOthers] when its removal took others, if alone is set.
func (w WorkWriter) RestoreTask(ctx context.Context, opID, id, project string,
	alone bool, notify *worklet.Notify) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opRestoreTask, &w.actor, restoreTaskArgs{
		OpID: opID, ID: id, Project: project, Alone: alone, Notify: notify,
	})
	w.settled(out.Result)
	return out, err
}

// PurgeTask destroys a task and everything derived from it.
func (w WorkWriter) PurgeTask(ctx context.Context, opID, id, project, reason string) (
	worklet.WriteResult, error) {
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
func (p Pages) List(ctx context.Context, f doclet.Filter, fresh statelog.Freshness) (doclet.Listing, error) {
	return call(ctx, p.r, opListPages, nil, listPagesArgs{Filter: f, Fresh: fresh})
}

// Get answers one page.
func (p Pages) Get(ctx context.Context, ref string, fresh statelog.Freshness) (doclet.Detail, error) {
	return call(ctx, p.r, opGetPage, nil, getPageArgs{Ref: ref, Fresh: fresh})
}

// SkillPages is the tool-skill container, for the skill sync.
func (p Pages) SkillPages(ctx context.Context, container string,
	fresh statelog.Freshness) ([]doclet.Page, error) {
	return call(ctx, p.r, opSkillPages, nil, skillPagesArgs{Container: container, Fresh: fresh})
}

// Containers lists every container.
func (p Pages) Containers(ctx context.Context, fresh statelog.Freshness) (
	[]doclet.ContainerListing, error) {
	return call(ctx, p.r, opContainers, nil, containersArgs{Fresh: fresh})
}

// Activity answers what happened to a page, a container or the whole
// knowledge base.
func (p Pages) Activity(ctx context.Context, q doclet.PageActivityQuery) (doclet.PageActivity, error) {
	return call(ctx, p.r, opPageActivity, nil, q)
}

// Revision answers one revision of a page's body, and false where the page
// has none at that version.
func (p Pages) Revision(ctx context.Context, pageID string, version int,
	fresh statelog.Freshness) (doclet.Revision, bool, error) {
	out, err := call(ctx, p.r, opRevision, nil, revisionArgs{PageID: pageID, Version: version, Fresh: fresh})
	return out.Revision, out.Found, err
}

// Create writes a new page. Never repeated when unanswered — see
// [ErrOutcomeUnknown] — unless its actor carries the caller's operation key,
// under which a repeat is the same operation ([op.repeatableWhen]). So for
// every page write below; a comment may carry its turn's key instead.
func (p Pages) Create(ctx context.Context, actor doclet.Actor, in doclet.NewPage) (doclet.Written, error) {
	out, err := call(ctx, p.r, opCreatePage, nil, createPageArgs{Actor: actor, Page: in})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// SavePage saves a page's body.
func (p Pages) SavePage(ctx context.Context, actor doclet.Actor, pageID string,
	save doclet.Save) (doclet.Written, error) {
	out, err := call(ctx, p.r, opSavePage, nil, savePageArgs{Actor: actor, PageID: pageID, Save: save})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// Rename moves a page to a new title.
func (p Pages) Rename(ctx context.Context, actor doclet.Actor, pageID, title string,
	quiet bool) (doclet.Written, error) {
	out, err := call(ctx, p.r, opRenamePage, nil, renamePageArgs{
		Actor: actor, PageID: pageID, Title: title, Quiet: quiet,
	})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// Comment adds a comment.
func (p Pages) Comment(ctx context.Context, actor doclet.Actor, pageID string,
	in doclet.NewComment) (doclet.Comment, doclet.Written, error) {
	out, err := call(ctx, p.r, opCommentPage, nil, commentArgs{Actor: actor, PageID: pageID, Comment: in})
	p.r.Observe(out.Written.Outcome.Position)
	return out.Comment, out.Written, err
}

// EditComment rewrites a comment.
func (p Pages) EditComment(ctx context.Context, actor doclet.Actor, pageID, commentID,
	body string) (doclet.Comment, doclet.Written, error) {
	out, err := call(ctx, p.r, opEditComment, nil, editCommentArgs{
		Actor: actor, PageID: pageID, CommentID: commentID, Body: body,
	})
	p.r.Observe(out.Written.Outcome.Position)
	return out.Comment, out.Written, err
}

// Trash moves a page to the trash.
func (p Pages) Trash(ctx context.Context, actor doclet.Actor, pageID string) (doclet.Written, error) {
	out, err := call(ctx, p.r, opTrashPage, nil, pageRefArgs{Actor: actor, PageID: pageID})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// Restore brings a page back out of the trash.
func (p Pages) Restore(ctx context.Context, actor doclet.Actor, pageID string) (doclet.Written, error) {
	out, err := call(ctx, p.r, opRestorePage, nil, pageRefArgs{Actor: actor, PageID: pageID})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// Purge destroys a page and everything derived from it.
func (p Pages) Purge(ctx context.Context, actor doclet.Actor, pageID, reason string) (
	doclet.Written, error) {
	out, err := call(ctx, p.r, opPurgePage, nil, purgePageArgs{
		Actor: actor, PageID: pageID, Reason: reason,
	})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// RemoveComment takes one remark down, under the authority the caller's table
// decided.
func (p Pages) RemoveComment(ctx context.Context, actor doclet.Actor, pageID, commentID string,
	authority doclet.CommentAuthority) (doclet.Written, error) {
	out, err := call(ctx, p.r, opRemoveComment, nil, removeCommentArgs{
		Actor: actor, PageID: pageID, CommentID: commentID, Authority: authority,
	})
	p.r.Observe(out.Outcome.Position)
	return out, err
}

// Chat is the engine's own chat, read: every read answered for the one viewer
// it names, by a data node whose copy serves.
type Chat struct{ r *Router }

// Chat answers the engine's own chat.
func (r *Router) Chat() Chat { return Chat{r: r} }

var _ chatlet.ThreadSource = Chat{}

// Channels answers the rooms a viewer is in.
func (c Chat) Channels(ctx context.Context, viewer string, q chatlet.ChannelsQuery,
	fresh statelog.Freshness) (chatlet.ChannelListing, error) {
	return call(ctx, c.r, opChatChannels, nil, chatChannelsArgs{Viewer: viewer, Query: q, Fresh: fresh})
}

// Channel answers one room, for a viewer who may read it.
func (c Chat) Channel(ctx context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {
	return call(ctx, c.r, opChatChannel, nil,
		chatChannelArgs{Viewer: viewer, ChannelID: channelID, Fresh: fresh})
}

// ChannelHere is [Chat.Channel] asked of THIS node's own copy first, even
// while that copy lags its log — see [callHere].
//
// FOR THE LIVE CHANNEL'S DECISION ABOUT A RECORD THIS NODE'S APPLIER HAS JUST
// COMMITTED: whether one viewer may be shown it. That copy holds the record by
// construction, while the ordinary walk would ask a peer first whenever the
// copy lags — during the very burst that makes it lag — turning every
// decision into a round trip to a node that may not have applied the record
// yet. A copy out of service is passed over as ever, and the caller's floor
// ([statelog.Freshness.MinPosition]) holds whichever copy answers to the
// record being announced.
func (c Chat) ChannelHere(ctx context.Context, viewer, channelID string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {
	return callHere(ctx, c.r, opChatChannel, nil,
		chatChannelArgs{Viewer: viewer, ChannelID: channelID, Fresh: fresh})
}

// ChannelNamed answers the room a name addresses, for a viewer who may read it.
func (c Chat) ChannelNamed(ctx context.Context, viewer, name string,
	fresh statelog.Freshness) (chatlet.ChannelDetail, error) {
	return call(ctx, c.r, opChatChannelNamed, nil,
		chatChannelNamedArgs{Viewer: viewer, Name: name, Fresh: fresh})
}

// Messages answers a page of one room's transcript.
func (c Chat) Messages(ctx context.Context, viewer string, q chatlet.TranscriptQuery,
	fresh statelog.Freshness) (chatlet.Transcript, error) {
	return call(ctx, c.r, opChatMessages, nil, chatMessagesArgs{Viewer: viewer, Query: q, Fresh: fresh})
}

// Thread answers one thread, its root and its replies.
func (c Chat) Thread(ctx context.Context, viewer string, q chatlet.ThreadQuery,
	fresh statelog.Freshness) (chatlet.Thread, error) {
	return call(ctx, c.r, opChatThread, nil, chatThreadArgs{Viewer: viewer, Query: q, Fresh: fresh})
}

// Mentions answers a page of the messages that named a viewer.
func (c Chat) Mentions(ctx context.Context, viewer string, q chatlet.MentionQuery,
	fresh statelog.Freshness) (chatlet.MentionFeed, error) {
	return call(ctx, c.r, opChatMentions, nil, chatMentionsArgs{Viewer: viewer, Query: q, Fresh: fresh})
}

// ChatSearch is the chat keyword search, answered for one viewer by a data
// node that holds both the rooms the viewer may read and the index.
type ChatSearch struct{ r *Router }

// ChatSearch answers the chat keyword search.
func (r *Router) ChatSearch() ChatSearch { return ChatSearch{r: r} }

// Search ranks the company's chat for one viewer, over the rooms that viewer
// may read.
//
// AN ERROR, NEVER AN EMPTY ANSWER, where no data node could run it — unlike the
// knowledge search, which is best effort by its seam's own contract. Nothing
// here is best effort: a seat told "nobody said that" acts on it, and a person
// searching a screen is shown the refusal rather than an empty list. An index
// still catching up says so on the answer ([ChatSearchAnswer.Building]).
func (s ChatSearch) Search(ctx context.Context, q ChatQuery) (ChatSearchAnswer, error) {
	return call(ctx, s.r, opChatSearch, nil, q)
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

// Knowledge is the Doclet knowledge search, answered by a data node's index.
type Knowledge struct{ r *Router }

// Knowledge answers the knowledge search.
func (r *Router) Knowledge() Knowledge { return Knowledge{r: r} }

var _ knowledge.Searcher = Knowledge{}

// Backend is Doclet's, searched on a node that holds it.
func (Knowledge) Backend() string { return doclet.Backend }

// CanSearch is the no-I/O pre-gate, and on Doclet every seat can
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
