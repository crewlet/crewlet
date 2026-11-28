package estate

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/knowledge"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The asking half: the seams every node's tools are handed, each answered by a
// data node whose copy of the operation's domain serves, this one where it
// does (ADR-0079). Each method keeps the in-process signature it stands in
// for, so a tool cannot tell which node answered: one that behaved differently
// on a node holding no data would be two tools, only one of them tested.

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

// Views answers one container's strip of views, to show.
func (w Work) Views(ctx context.Context, q worklet.ViewQuery) (worklet.ViewListing, error) {
	return w.strip(ctx, opViews, q)
}

// CarriedViews answers one container's strip of views as a caller saves a view
// of it again from, each page read whole or refused ([ErrUnknownKeys]).
func (w Work) CarriedViews(ctx context.Context, q worklet.ViewQuery) (worklet.ViewListing, error) {
	return w.strip(ctx, opViewsCarried, q)
}

// ViewsPage answers one page of a container's strip.
func (w Work) ViewsPage(ctx context.Context, q worklet.ViewQuery, after string) (worklet.ViewListing, string, error) {
	page, err := call(ctx, w.r, opViews, nil, viewsArgs{Query: q, Zone: zoneArg(q.Zone), After: after})
	return page.Listing, page.Next, err
}

// EveryViewPage answers one page of every view a person sees.
func (w Work) EveryViewPage(ctx context.Context, q worklet.EveryViewQuery,
	after string) (worklet.ViewListing, string, error) {
	page, err := call(ctx, w.r, opEveryView, nil, everyViewArgs{Query: q, Zone: zoneArg(q.Zone), After: after})
	return page.Listing, page.Next, err
}

// strip is a container's strip a page at a time, each page one answer: the
// implicit views, then the saved ones with the viewer's pins first, as one
// read orders them; complete only where every page was.
func (w Work) strip(ctx context.Context, o op[viewsArgs, viewsPage], q worklet.ViewQuery) (worklet.ViewListing, error) {
	var out worklet.ViewListing
	var implicit, saved []worklet.ViewRow
	complete, incomplete := true, (*worklet.Incomplete)(nil)
	for after := ""; ; {
		page, err := call(ctx, w.r, o, nil, viewsArgs{Query: q, Zone: zoneArg(q.Zone), After: after})
		if err != nil {
			return worklet.ViewListing{}, err
		}
		for _, row := range page.Listing.Views {
			if row.Builtin {
				implicit = append(implicit, row)
			} else {
				saved = append(saved, row)
			}
		}
		out = page.Listing
		if complete = complete && page.Listing.Complete; page.Listing.Incomplete != nil {
			incomplete = page.Listing.Incomplete
		}
		if after = page.Next; after == "" {
			implicit = append(implicit, worklet.PinnedFirst(saved)...)
			out.Views = implicit
			out.Complete, out.Incomplete = complete, incomplete
			return out, nil
		}
	}
}

// ExpandedQuery resolves a saved view's parameters into a query.
func (w Work) ExpandedQuery(ctx context.Context, params map[string]any,
	viewer worklet.Viewer, now time.Time, loc *time.Location) (worklet.Query, error) {
	return call(ctx, w.r, opExpandedQuery, nil,
		expandArgs{Params: params, Viewer: viewer, Now: now, Zone: zoneArg(loc)})
}

// Catalogue answers the workspace catalogue, to show.
func (w Work) Catalogue(ctx context.Context, q worklet.CatalogueQuery) (worklet.CatalogueAnswer, error) {
	return call(ctx, w.r, opCatalogue, nil, q)
}

// CarriedCatalogue answers the workspace catalogue a caller sends back whole,
// read whole or refused ([ErrUnknownKeys]).
func (w Work) CarriedCatalogue(ctx context.Context, q worklet.CatalogueQuery) (worklet.CatalogueAnswer, error) {
	return call(ctx, w.r, opCatalogueCarried, nil, q)
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

// FlowHours answers how long each seat's items in progress stood in each flow
// state over a window (ADR-0162's flow record).
func (w Work) FlowHours(ctx context.Context, q worklet.FlowHoursQuery) ([]worklet.FlowHours, error) {
	return call(ctx, w.r, opFlowHours, nil, q)
}

// FlowItems answers how many items in progress each seat had in a window.
func (w Work) FlowItems(ctx context.Context, q worklet.FlowHoursQuery) (map[string]int, error) {
	return call(ctx, w.r, opFlowItems, nil, q)
}

// FlowPickups answers how long each item picked up in a window had stood due.
func (w Work) FlowPickups(ctx context.Context, q worklet.FlowHoursQuery) ([]time.Duration, error) {
	return call(ctx, w.r, opFlowPickups, nil, q)
}

// WorkPatterns answers what the company's comments and answers did in a window.
func (w Work) WorkPatterns(ctx context.Context, q worklet.PatternsQuery) (worklet.WorkPatterns, error) {
	return call(ctx, w.r, opWorkPatterns, nil, q)
}

// TurnsMoved answers which of a set of turns moved work on the tracker.
func (w Work) TurnsMoved(ctx context.Context, q worklet.TurnsMovedQuery) (map[string]bool, error) {
	return call(ctx, w.r, opTurnsMoved, nil, q)
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

// ReadyWork answers what one seat could start now: the work pickup's read
// (ADR-0112), from whichever data node serves the tracker.
func (w Work) ReadyWork(ctx context.Context, q worklet.ReadyWorkQuery, now time.Time) (
	worklet.ReadyWork, error) {
	return call(ctx, w.r, opReadyWork, nil, readyWorkArgs{Query: q, Now: now})
}

// PickupAnswer answers how a seat's turn left the item a work pickup handed it
// (ADR-0122), from whichever data node serves the tracker.
func (w Work) PickupAnswer(ctx context.Context, q worklet.PickupAnswerQuery) (types.PickupAnswer, error) {
	return call(ctx, w.r, opPickupAnswer, nil, q)
}

// OwnAsks answers a seat's open asks to one person: what a turn the seat
// holds with that person is handed at its start.
func (w Work) OwnAsks(ctx context.Context, q worklet.OwnAsksQuery, now time.Time) (worklet.OwnAsks, error) {
	return call(ctx, w.r, opOwnAsks, nil, ownAsksArgs{Query: q, Now: now})
}

// EntryMarks answers how many open tasks name each atlas entry (ADR-0140).
func (w Work) EntryMarks(ctx context.Context, level statelog.ReadLevel) (worklet.EntryMarks, error) {
	return call(ctx, w.r, opEntryMarks, nil, level)
}

// EntryWork answers the open and recently finished tasks naming an atlas entry.
func (w Work) EntryWork(ctx context.Context, q worklet.EntryWorkQuery, now time.Time) (worklet.EntryWork, error) {
	return call(ctx, w.r, opEntryWork, nil, entryWorkArgs{Query: q, Now: now})
}

// Projects lists the company's projects.
func (w Work) Projects(ctx context.Context, q worklet.ProjectQuery) (worklet.ProjectListing, error) {
	return call(ctx, w.r, opProjects, nil, q)
}

// Project describes one project, to show.
func (w Work) Project(ctx context.Context, q worklet.ProjectDetailQuery) (worklet.ProjectDetail, error) {
	return call(ctx, w.r, opProject, nil, q)
}

// CarriedProject describes one project whose declarations a caller sends back
// whole, read whole or refused ([ErrUnknownKeys]).
func (w Work) CarriedProject(ctx context.Context, q worklet.ProjectDetailQuery) (worklet.ProjectDetail, error) {
	return call(ctx, w.r, opProjectCarried, nil, q)
}

// Search is the tracker's ranked search, answered by a data node's index. A
// search no node could run is an error, never an empty answer: "nothing
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
	var out worklet.ViewListing
	var views []worklet.ViewRow
	complete, incomplete := true, (*worklet.Incomplete)(nil)
	for after := ""; ; {
		page, err := call(ctx, w.r, opEveryView, nil, everyViewArgs{Query: q, Zone: zoneArg(q.Zone), After: after})
		if err != nil {
			return worklet.ViewListing{}, err
		}
		views = append(views, page.Listing.Views...)
		out, out.Views = page.Listing, views
		if complete = complete && page.Listing.Complete; page.Listing.Incomplete != nil {
			incomplete = page.Listing.Incomplete
		}
		if after = page.Next; after == "" {
			// THE PINS FIRST over every page's rows, as one read orders them;
			// complete only where every page was.
			out.Views, out.Complete, out.Incomplete = worklet.PinnedFirst(out.Views), complete, incomplete
			return out, nil
		}
	}
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
// read, on whichever node answers it, includes it.
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

// Depend writes a dependency. The lead map passed here does not cross: the
// serving node's own is used (package doc, "What does not cross the wire, and
// who supplies it").
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

// DeleteView takes a view away, refused where the stored view is no longer prior.
func (w WorkWriter) DeleteView(ctx context.Context, opID, id string,
	prior worklet.ViewPrior) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opDeleteView, &w.actor,
		deleteViewArgs{OpID: opID, ID: id, Prior: prior})
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
// caller's table decided, held to seen (how far the caller read the Worklet's
// log) on whichever holder decides it.
func (w WorkWriter) MarkInbox(ctx context.Context, opID, handle string, seen statelog.Position,
	gesture inbox.Gesture[worklet.Reason], authority worklet.PersonAuthority) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opMarkInbox, &w.actor, markInboxArgs{
		OpID: opID, Handle: handle, Seen: seen, Gesture: gesture, Authority: authority,
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

// ResolveComment resolves the writer's own remark on a work item: an open
// question its asker withdraws, saying why where withdrawal is set.
func (w WorkWriter) ResolveComment(ctx context.Context, opID, taskID, project, commentID string,
	withdrawal *worklet.Withdrawal, notify *worklet.Notify) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opResolveTaskComment, &w.actor, resolveCommentOnTaskArgs{
		OpID: opID, TaskID: taskID, Project: project, CommentID: commentID,
		Withdrawal: withdrawal, Notify: notify,
	})
	w.settled(out.Result)
	return out, err
}

// RemoveComment takes a remark on a work item down, as its author or as a
// moderator.
func (w WorkWriter) RemoveComment(ctx context.Context, opID, taskID, project, commentID string,
	authority worklet.CommentAuthority) (worklet.WriteResult, error) {
	out, err := call(ctx, w.r, opRemoveTaskComment, &w.actor, removeCommentOnTaskArgs{
		OpID: opID, TaskID: taskID, Project: project, CommentID: commentID,
		Authority: authority,
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

// RestoreTask brings a task back out of the trash; with alone set, it is
// refused with [worklet.ErrRestoresOthers] when its removal took others.
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
	var out []doclet.Page
	for after := ""; ; {
		page, err := call(ctx, p.r, opSkillPages, nil, skillPagesArgs{Container: container, After: after,
			Fresh: fresh})
		if err != nil {
			return nil, err
		}
		out = append(out, page.Pages...)
		if after = page.Next; after == "" {
			return out, nil
		}
	}
}

// SkillPagesPage answers one page of a container's skill pages.
func (p Pages) SkillPagesPage(ctx context.Context, container, after string,
	fresh statelog.Freshness) ([]doclet.Page, string, error) {
	page, err := call(ctx, p.r, opSkillPages, nil, skillPagesArgs{Container: container, After: after, Fresh: fresh})
	return page.Pages, page.Next, err
}

// ContainersPage answers one page of containers.
func (p Pages) ContainersPage(ctx context.Context, after string,
	fresh statelog.Freshness) ([]doclet.ContainerListing, string, error) {
	page, err := call(ctx, p.r, opContainers, nil, containersArgs{After: after, Fresh: fresh})
	return page.Containers, page.Next, err
}

// Containers lists every container, a page at a time.
func (p Pages) Containers(ctx context.Context, fresh statelog.Freshness) (
	[]doclet.ContainerListing, error) {
	out := []doclet.ContainerListing{}
	for after := ""; ; {
		page, err := call(ctx, p.r, opContainers, nil, containersArgs{After: after, Fresh: fresh})
		if err != nil {
			return nil, err
		}
		out = append(out, page.Containers...)
		if after = page.Next; after == "" {
			return out, nil
		}
	}
}

// Activity answers what happened to a page, a container or the whole
// knowledge base.
func (p Pages) Activity(ctx context.Context, q doclet.PageActivityQuery) (doclet.PageActivity, error) {
	return call(ctx, p.r, opPageActivity, nil, q)
}

// TurnsMovedPages answers which of a set of turns moved work on the pages.
func (p Pages) TurnsMovedPages(ctx context.Context, q doclet.TurnsMovedQuery) (map[string]bool, error) {
	return call(ctx, p.r, opPageTurnsMoved, nil, q)
}

// LinkedFrom answers which pages and tasks link to a page, by its id, from a
// data node whose copies of both the knowledge base and the tracker serve, or
// [search.ErrIndexBuilding] where its index has not finished its first lap.
func (p Pages) LinkedFrom(ctx context.Context, pageID string) (search.Backlinks, error) {
	return call(ctx, p.r, opLinkedFrom, nil, linkedFromArgs{PageID: pageID})
}

// Revision answers one revision of a page's body, and false where the page
// has none at that version.
func (p Pages) Revision(ctx context.Context, pageID string, version int,
	fresh statelog.Freshness) (doclet.Revision, bool, error) {
	out, err := call(ctx, p.r, opRevision, nil, revisionArgs{PageID: pageID, Version: version, Fresh: fresh})
	return out.Revision, out.Found, err
}

// Create writes a new page. Like every page write below, it is never repeated
// when unanswered ([ErrOutcomeUnknown]) unless its actor carries the caller's
// operation key, under which a repeat is the same operation
// ([op.repeatableWhen]); a comment may carry its turn's key instead.
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

// Chat is the engine's own chat, read and written: every read answered for one
// viewer, every write as the actor it names.
//
// The actor, and the caller's key it carries ([chatlet.Actor.OpKey]), travel
// per call as on [Pages] rather than being bound into a writer as on
// [WorkWriter], because one node's chat store serves every seat and person.
//
// Every write raises this node's floor on the chat log to the position it
// reported, an error included: an unvouched or refused answer can still carry
// a position the next read must include. Tracker and knowledge reads never
// wait on it.
type Chat struct{ r *Router }

// Chat answers the engine's own chat.
func (r *Router) Chat() Chat { return Chat{r: r} }

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

// ChannelHere is [Chat.Channel] asked of this node's own copy first, even
// while it lags its log ([callHere]), for the live channel's decision whether
// a viewer may be shown a record this node's applier just committed. That copy
// holds the record by construction; the ordinary walk would ask a peer first
// whenever the copy lags, during the very burst that makes it lag, and that
// peer may not have applied the record yet. A copy out of service is still
// passed over, and the caller's floor ([statelog.Freshness.Floors]) holds
// whichever copy answers to the announced record.
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

// TurnThreads is the engine's own turn-start thread read: a thread with whom
// each message was written for ([chatlet.TurnThread]), so a turn works through
// only the waiting messages written for whom it works for (ADR-0109).
//
// A facade of its own, never a method on [Chat]: the API, the assistant and
// the agents' tools hold [Chat], and no reader may see whom a message was
// written for. The engine-class guard in this package's tests refuses it, and
// every road to it, in the packages that serve a person or a model.
type TurnThreads struct{ r *Router }

// TurnThreads is the engine's own turn-start thread read ([TurnThreads]).
func (r *Router) TurnThreads() TurnThreads { return TurnThreads{r: r} }

var _ chatlet.ThreadSource = TurnThreads{}

// TurnThread answers one thread with whom each of its messages was written
// for, falling back to the plain thread where no data node ran the turn's read
// and none went silent ([servedByNobody]): an older data node in a rolling
// upgrade answers the undeclared operation unserved. The plain thread says it
// carried nothing ([chatlet.TurnThread.Carried]), so the turn marks no waiting
// message worked through. Every other refusal is returned as it is.
//
// The fallback is broader than the roll: an unserved refusal carries no
// structured reason, so this cannot tell "no node knows the operation" from
// "every node is behind or out of service", and a degraded estate is asked
// twice. At most one read runs: the second is asked only after none ran the
// first.
func (t TurnThreads) TurnThread(ctx context.Context, viewer string, q chatlet.ThreadQuery,
	fresh statelog.Freshness) (chatlet.TurnThread, error) {

	args := chatThreadArgs{Viewer: viewer, Query: q, Fresh: fresh}
	out, err := call(ctx, t.r, opChatTurnThread, nil, args)
	if !servedByNobody(err) {
		return out, err
	}
	thread, err := call(ctx, t.r, opChatThread, nil, args)
	if err != nil {
		return chatlet.TurnThread{}, err
	}
	return chatlet.TurnThread{Thread: thread}, nil
}

// servedByNobody reports whether err is a walk in which no data node ran the
// operation and none went silent ([ErrUnserved]), and nothing else: never an
// [ErrUnknownKeys], which unwraps to the walk it found unserved.
func servedByNobody(err error) bool {
	var unknown *ErrUnknownKeys
	if err == nil || errors.As(err, &unknown) {
		return false
	}
	var unserved *ErrUnserved
	return errors.As(err, &unserved) && !unserved.Silent()
}

// RoomNames answers the names of every room a viewer may read.
func (c Chat) RoomNames(ctx context.Context, viewer string,
	fresh statelog.Freshness) (chatlet.RoomNames, error) {
	return call(ctx, c.r, opChatRoomNames, nil, chatRoomNamesArgs{Viewer: viewer, Fresh: fresh})
}

// Directory answers every named room a viewer may browse.
func (c Chat) Directory(ctx context.Context, viewer string,
	fresh statelog.Freshness) (chatlet.Directory, error) {
	return call(ctx, c.r, opChatDirectory, nil, chatRoomNamesArgs{Viewer: viewer, Fresh: fresh})
}

// settled raises the session floor to what a chat write reported.
func (c Chat) settled(w chatlet.Written) { c.r.Observe(w.Outcome.Position) }

// CreateChannel opens a room. Like every chat write below but the erase and the
// prune (which take the caller's operation id), it is never repeated when
// unanswered ([ErrOutcomeUnknown]) unless its actor carries the caller's
// operation key ([op.repeatableWhen]).
func (c Chat) CreateChannel(ctx context.Context, actor chatlet.Actor, in chatlet.NewChannel) (
	chatlet.Written, error) {
	out, err := call(ctx, c.r, opChatCreateChannel, nil, chatCreateChannelArgs{Actor: actor, Channel: in})
	c.settled(out)
	return out, err
}

// OpenDirect opens the direct conversation between the actor and the
// participants, reporting whether this call created it.
func (c Chat) OpenDirect(ctx context.Context, actor chatlet.Actor, participants []string) (
	chatlet.Written, bool, error) {
	out, err := call(ctx, c.r, opChatOpenDirect, nil,
		chatOpenDirectArgs{Actor: actor, Participants: participants})
	c.settled(out.Written)
	return out.Written, out.Created, err
}

// PatchChannel changes a room's topic, purpose or archive, under the authority
// the caller's table decided.
func (c Chat) PatchChannel(ctx context.Context, actor chatlet.Actor, channelID string,
	patch chatlet.ChannelPatch, authority chatlet.ChannelAuthority) (chatlet.Written, error) {
	out, err := call(ctx, c.r, opChatPatchChannel, nil, chatPatchChannelArgs{
		Actor: actor, ChannelID: channelID, Patch: patch, Authority: authority,
	})
	c.settled(out)
	return out, err
}

// SetMembers replaces a room's members, against the membership the caller
// read where it names one.
func (c Chat) SetMembers(ctx context.Context, actor chatlet.Actor, channelID string,
	members []chatlet.Member, expected *[]chatlet.Member) (chatlet.Written, error) {
	out, err := call(ctx, c.r, opChatSetMembers, nil, chatSetMembersArgs{
		Actor: actor, ChannelID: channelID, Members: members, Expected: expected,
	})
	c.settled(out)
	return out, err
}

// AddMembers puts people in a room, answering who it put there.
func (c Chat) AddMembers(ctx context.Context, actor chatlet.Actor, channelID string,
	handles []string) (chatlet.Written, []string, error) {
	out, err := call(ctx, c.r, opChatAddMembers, nil,
		chatAddMembersArgs{Actor: actor, ChannelID: channelID, Handles: handles})
	c.settled(out.Written)
	return out.Written, out.Added, err
}

// Join puts the actor in a room.
func (c Chat) Join(ctx context.Context, actor chatlet.Actor, channelID string) (chatlet.Written, error) {
	out, err := call(ctx, c.r, opChatJoin, nil, chatMembershipArgs{Actor: actor, ChannelID: channelID})
	c.settled(out)
	return out, err
}

// Leave takes the actor out of a room.
func (c Chat) Leave(ctx context.Context, actor chatlet.Actor, channelID string) (chatlet.Written, error) {
	out, err := call(ctx, c.r, opChatLeave, nil, chatMembershipArgs{Actor: actor, ChannelID: channelID})
	c.settled(out)
	return out, err
}

// Post says something in a room.
func (c Chat) Post(ctx context.Context, actor chatlet.Actor, channelID string,
	in chatlet.NewMessage) (chatlet.Written, error) {
	out, err := call(ctx, c.r, opChatPost, nil,
		chatPostArgs{Actor: actor, ChannelID: channelID, Message: in})
	c.settled(out)
	return out, err
}

// Reply says something in a message's thread.
func (c Chat) Reply(ctx context.Context, actor chatlet.Actor, channelID, replyTo string,
	in chatlet.NewMessage) (chatlet.Written, error) {
	out, err := call(ctx, c.r, opChatReply, nil,
		chatReplyArgs{Actor: actor, ChannelID: channelID, ReplyTo: replyTo, Message: in})
	c.settled(out)
	return out, err
}

// Reach answers whom a post or a reply would reach if it were sent now,
// without sending it: the post's own decision, run dry on a data node.
func (c Chat) Reach(ctx context.Context, actor chatlet.Actor, channelID, replyTo string,
	in chatlet.NewMessage) (chatlet.Draft, error) {
	return call(ctx, c.r, opChatReach, nil,
		chatReachArgs{Actor: actor, ChannelID: channelID, ReplyTo: replyTo, Message: in})
}

// Edit rewrites a message its actor wrote.
func (c Chat) Edit(ctx context.Context, actor chatlet.Actor, channelID, messageID string,
	in chatlet.EditMessage) (chatlet.Written, error) {
	out, err := call(ctx, c.r, opChatEdit, nil,
		chatEditArgs{Actor: actor, ChannelID: channelID, MessageID: messageID, Edit: in})
	c.settled(out)
	return out, err
}

// Delete takes a message down, leaving its tombstone, under the authority the
// caller's table decided.
func (c Chat) Delete(ctx context.Context, actor chatlet.Actor, channelID, messageID string,
	authority chatlet.MessageAuthority) (chatlet.Written, error) {
	out, err := call(ctx, c.r, opChatDelete, nil, chatDeleteArgs{
		Actor: actor, ChannelID: channelID, MessageID: messageID, Authority: authority,
	})
	c.settled(out)
	return out, err
}

// React adds the actor's reaction to a message.
func (c Chat) React(ctx context.Context, actor chatlet.Actor, channelID, messageID,
	emoji string) (chatlet.Written, error) {
	out, err := call(ctx, c.r, opChatReact, nil, chatReactArgs{
		Actor: actor, ChannelID: channelID, MessageID: messageID, Emoji: emoji,
	})
	c.settled(out)
	return out, err
}

// Unreact takes the actor's reaction back.
func (c Chat) Unreact(ctx context.Context, actor chatlet.Actor, channelID, messageID,
	emoji string) (chatlet.Written, error) {
	out, err := call(ctx, c.r, opChatReact, nil, chatReactArgs{
		Actor: actor, ChannelID: channelID, MessageID: messageID, Emoji: emoji, Remove: true,
	})
	c.settled(out)
	return out, err
}

// TakeReceipt records on handle's receipt for a message that its running turn
// took it, as the engine ([chatlet.Store.TakeReceipt]).
func (c Chat) TakeReceipt(ctx context.Context, actor chatlet.Actor, channelID, messageID, handle string,
	as chatlet.Taken) (chatlet.Written, error) {
	out, err := call(ctx, c.r, opChatTake, nil, chatTakeArgs{
		Actor: actor, ChannelID: channelID, MessageID: messageID, Handle: handle, As: as,
	})
	c.settled(out)
	return out, err
}

// SetRetention states how long a room keeps what is said in it: days, or nil
// for the company's default.
func (c Chat) SetRetention(ctx context.Context, actor chatlet.Actor, channelID string,
	days *int) (chatlet.Written, error) {
	out, err := call(ctx, c.r, opChatSetRetention, nil,
		chatSetRetentionArgs{Actor: actor, ChannelID: channelID, Days: days})
	c.settled(out)
	return out, err
}

// Erase removes messages for good, under the operation id the caller derived,
// repeated under it when unanswered as a purge is.
func (c Chat) Erase(ctx context.Context, actor chatlet.Actor, opID, channelID string,
	messageIDs []string, reason string) (chatlet.Written, error) {
	out, err := call(ctx, c.r, opChatErase, nil, chatEraseArgs{
		Actor: actor, OpID: opID, ChannelID: channelID, MessageIDs: messageIDs, Reason: reason,
	})
	c.settled(out)
	return out, err
}

// Prune drops a room's messages older than cutoff, under the operation id the
// caller derived.
func (c Chat) Prune(ctx context.Context, actor chatlet.Actor, opID, channelID string,
	cutoff time.Time) (chatlet.Written, error) {
	out, err := call(ctx, c.r, opChatPrune, nil, chatPruneArgs{
		Actor: actor, OpID: opID, ChannelID: channelID, Cutoff: cutoff,
	})
	c.settled(out)
	return out, err
}

// ChatSearch is the chat keyword search, answered for one viewer by a data
// node that holds both the rooms the viewer may read and the index.
type ChatSearch struct{ r *Router }

// ChatSearch answers the chat keyword search.
func (r *Router) ChatSearch() ChatSearch { return ChatSearch{r: r} }

// Search ranks the company's chat for one viewer, over the rooms that viewer
// may read. Where no data node could run it, it is an error, never an empty
// answer as the best-effort knowledge search gives: a seat told "nobody said
// that" acts on it. An index still catching up says so
// ([ChatSearchAnswer.Building]).
func (s ChatSearch) Search(ctx context.Context, q ChatQuery) (ChatSearchAnswer, error) {
	return call(ctx, s.r, opChatSearch, nil, q)
}

// Identity is the identity directory's read side, answered by a data node for
// a node holding no data ([IdentityReader]).
type Identity struct{ r *Router }

// Identity answers the identity directory's reads.
func (r *Router) Identity() Identity { return Identity{r: r} }

// BindingByLogin answers the directory row a login binds, narrowed to the
// binding ([Binding]), and how far the node that answered could vouch for it.
// A login nobody holds is the zero binding and no error. The vouch is the
// answering node's, whose rows the binding was read from, and the caller's
// table weighs it as it would its own.
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

// SeatHoldersHeld is [Identity.SeatHolders]: the routed answer is the held one.
func (i Identity) SeatHoldersHeld(ctx context.Context) ([]iamdomain.SeatHolder, error) {
	return i.SeatHolders(ctx)
}

// Requester answers who holds one seat, with the holder's grants, and whether
// anybody does, read in one snapshot on the node that answered:
// [Identity.SeatHolders]' answer for that seat alone, as two seeks. A seat's
// turn composes the person it acts for from it. An error is UNKNOWN, never a
// seat nobody holds; a node that does not know the question leaves it
// unserved, and none answering is an error.
func (i Identity) Requester(ctx context.Context, seat string) (iamdomain.Requester, bool, error) {
	out, err := call(ctx, i.r, opRequester, nil, seatArgs{Seat: seat})
	return out.Requester, out.Held, err
}

// MachineToken answers the row a presented machine token is checked against
// ([credential.CheckToken]'s input), as a data node holds it, for a node that
// holds no identity rows of its own. The presented token stays here: only its
// id, a fresh nonce and the keyring entry cross, and the row comes back with a
// MAC in its verifier's place ([TokenFacts]) (package doc, "What does not
// cross the wire, and who supplies it"). A token named by id alone, a view
// ticket's, is answered with no verifier, which [credential.CheckHeld] never
// reads. The vouch is the answering node's, as [Identity.BindingByLogin]'s is.
// An error is always the unknown arm: no row read, no node answering, or no
// key here to check an answer under is "cannot say", never "no such token".
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

// Knowledge is the Doclet knowledge search, answered by a data node's index.
type Knowledge struct{ r *Router }

// Knowledge answers the knowledge search.
func (r *Router) Knowledge() Knowledge { return Knowledge{r: r} }

var _ knowledge.Searcher = Knowledge{}

// Backend is Doclet's, searched on a node that holds it.
func (Knowledge) Backend() string { return doclet.Backend }

// CanSearch is the no-I/O pre-gate: always true, since on Doclet every seat
// can read every page.
func (Knowledge) CanSearch(*org.Role, *org.Organization) bool { return true }

// Search is best effort, as the seam requires: a failure is logged and answers
// no hits, so a turn does not die because the node holding the index was slow.
// But it says it did not run ([notSearched]), never "nothing matched": a seat
// told the company wrote nothing down would write a duplicate of a page that
// exists.
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
// no mode served and every bucket missing, which every reader renders as
// "could not be searched".
func notSearched() knowledge.Result {
	return knowledge.Result{Outcome: knowledge.Outcome{
		Coverage: knowledge.Coverage{
			Nodes: []knowledge.NodeCoverage{}, BucketsMissing: search.SearchShards,
		},
	}}
}

// Building reports whether the index a search reads is still building, so an
// empty block reads "still indexing". False when nothing answers.
func (k Knowledge) Building(ctx context.Context) bool {
	building, err := call(ctx, k.r, opKnowledgeBuilding, nil, struct{}{})
	return err == nil && building
}

// Usage is the usage domain's reads (ADR-0020), answered by a data node whose
// usage copy is in service, this node's own where it is one (ADR-0079).
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

// allPages reads every page of one paged read, in order, each page its own
// routed call: a failover between pages resumes on another data node from the
// same group, since every data node pages by one rule ([usage.Fill]). A page
// that does not end after the one before is refused, or a node paging another
// way loops for ever.
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
			return nil, fmt.Errorf("estate: a usage page ended at %s, which is not after %s — "+
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

// ServerToolCalls answers every node's calls to one MCP server's tools over a
// range of company days, one row per (tool, seat).
func (u Usage) ServerToolCalls(ctx context.Context, q usage.ServerToolCallsQuery) (
	[]usage.ServerToolCallsRow, error) {
	return call(ctx, u.r, opUsageServerToolCalls, nil, q)
}

// TurnLedger answers every seat's turns by what woke them and what they did.
func (u Usage) TurnLedger(ctx context.Context, q usage.SpendQuery) ([]usage.TurnLedgerRow, error) {
	return call(ctx, u.r, opUsageTurnLedger, nil, q)
}

// ToolsElided answers how many (server, tool) entries the per-seat-day cap
// dropped over a range of company days, on every node.
func (u Usage) ToolsElided(ctx context.Context, from, to string) (int64, error) {
	return call(ctx, u.r, opUsageToolsElided, nil, readsElidedArgs{From: from, To: to})
}

// RepoReadersAnswer is one page of a repository's readers, summed across every
// node and day of the range ([usage.RepoReadsPage]).
type RepoReadersAnswer struct {
	// Readers are the page's readers, in reader order.
	Readers []usage.RepoReader

	// Elided is how many readers the per-day cap dropped over the range on
	// every node, on the first page alone; nil on every later page.
	Elided *int64

	// Next is the cursor of the page after this one, "" on the last.
	Next string
}

// RepoReaders answers one page of a repository's readers over a range of
// company days, cut at pageBytes and resuming at cursor ("" for the first
// page, else a page's [RepoReadersAnswer.Next]), with the first page's elided
// count. Each page is its own routed call, as in [allPages], so a walk across
// a failover reads every reader once. A bad cursor or range is refused
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
			return RepoReadersAnswer{}, fmt.Errorf("estate: a page of readers ended at %s, which is not "+
				"after %s — the answering node pages by another rule", page.Next, after)
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

// CodeSeam is Gitlet's reach into the estate, from every node: proposals to a
// repository's committer and writes to the change and checks writers beside
// it, and the reads admission, a cache's sync and the REST routes make, each
// answered by a data node whose code log copy serves; and a read by object id
// or an engine write, asked of the repository's home Gitlet node ([GitCopy]).
//
// Only this node's own writes that landed raise its floors: a read's position
// is somebody else's writes, and observing it would hold every later read this
// node routes, `stale` included, behind the newest linearizable one. A write
// raises the session's floor on the code log and, where it moved git content,
// the floor of the repository it wrote ([CodeSeam.FloorOf]); a conversation's
// write and a check's raise the session's alone.
type CodeSeam struct{ r *Router }

// Code answers Gitlet.
func (r *Router) Code() CodeSeam { return CodeSeam{r: r} }

// Propose asks the repository's committer to decide a push, and raises this
// node's floors past the record it landed in ([CodeSeam]).
func (c CodeSeam) Propose(ctx context.Context, p gitlet.Proposal) (gitlet.ProposeResult, error) {
	out, err := call(ctx, c.r, opCodePropose, nil, p)
	if out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending {
		c.wrote(p.RepoID, out.Position)
	}
	return out, err
}

// Compact asks the repository's committer to decide a compaction, and raises
// this node's floors past the record it landed in, which the compactor's next
// listing and the cache's sync after it must see.
func (c CodeSeam) Compact(ctx context.Context, p gitlet.CompactionProposal) (gitlet.CompactResult, error) {
	out, err := call(ctx, c.r, opCodeCompact, nil, p)
	if out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending {
		c.wrote(p.Compaction.RepoID, out.Position)
	}
	return out, err
}

// Expunge asks the repository's committer to decide an expunge record (a
// person's purge request, or a maintainer's begin or commit), and raises this
// node's floors past the record it landed in.
func (c CodeSeam) Expunge(ctx context.Context, p gitlet.ExpungeProposal) (gitlet.ExpungeResult, error) {
	out, err := call(ctx, c.r, opCodeExpunge, nil, p)
	if out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending {
		c.wrote(p.RepoID(), out.Position)
	}
	return out, err
}

// Import asks the repository's committer to decide one step of its import (a
// chunk or the import's end), and raises this node's floors past the record it
// landed in.
func (c CodeSeam) Import(ctx context.Context, p gitlet.ImportProposal) (gitlet.SyncResult, error) {
	out, err := call(ctx, c.r, opCodeImport, nil, p)
	if out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending {
		c.wrote(p.RepoID(), out.Position)
	}
	return out, err
}

// MirrorSync asks the repository's committer to decide one sync of a pull
// mirror, and raises this node's floors past the record it landed in.
func (c CodeSeam) MirrorSync(ctx context.Context, p gitlet.MirrorSyncProposal) (gitlet.SyncResult, error) {
	out, err := call(ctx, c.r, opCodeMirrorSync, nil, p)
	if out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending {
		c.wrote(p.RepoID(), out.Position)
	}
	return out, err
}

// Change asks the repository's committer to decide a write of a change's
// lifecycle, and raises this node's floors past the record it landed in. A
// write answered UNCHANGED raises nothing: the position it names is the
// change's last record, somebody else's write.
func (c CodeSeam) Change(ctx context.Context, p gitlet.ChangeProposal) (gitlet.ChangeResult, error) {
	out, err := call(ctx, c.r, opCodeChange, nil, p)
	if !out.Unchanged && (out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending) {
		c.wrote(p.RepoID(), out.Position)
	}
	return out, err
}

// Queue asks the repository's committer to decide a write of a target's merge
// queue, and raises this node's floors past the record it landed in.
func (c CodeSeam) Queue(ctx context.Context, p gitlet.QueueProposal) (gitlet.QueueResult, error) {
	out, err := call(ctx, c.r, opCodeQueue, nil, p)
	if out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending {
		c.wrote(p.RepoID(), out.Position)
	}
	return out, err
}

// Merge asks the repository's committer to decide a change's merge, and raises
// this node's floors past the record it landed in.
func (c CodeSeam) Merge(ctx context.Context, p gitlet.MergeProposal) (gitlet.MergeResult, error) {
	out, err := call(ctx, c.r, opCodeMerge, nil, p)
	if out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending {
		c.wrote(p.RepoID(), out.Position)
	}
	return out, err
}

// Converse asks the repository's committing data node to decide a write of a
// change's conversation, and raises this node's SESSION floor past the record
// it landed in. Never the repository's own floor, a git read's sync level: a
// conversation moves no ref and no pack, so raising it would make the Gitlet
// node sync for nothing. A write answered UNCHANGED raises nothing.
func (c CodeSeam) Converse(ctx context.Context, w gitlet.ConversationWrite) (gitlet.ConversationResult, error) {
	out, err := call(ctx, c.r, opCodeConversation, nil, w)
	if !out.Unchanged && (out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending) {
		c.r.Observe(out.Position)
	}
	return out, err
}

// Mark asks a data node to decide a person's gesture over their own code
// notices, or over the files of one change they read (gitlet.MarkWrite.Files),
// and raises this node's SESSION floor past the record it landed in. A mark
// answered UNCHANGED raises nothing.
func (c CodeSeam) Mark(ctx context.Context, w gitlet.MarkWrite) (gitlet.MarkResult, error) {
	out, err := call(ctx, c.r, opCodeMark, nil, w)
	if !out.Unchanged && (out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending) {
		c.r.Observe(out.Position)
	}
	return out, err
}

// Checks asks the repository's committing data node to decide a write of the
// repository's checks, and raises this node's SESSION floor past the record it
// landed in; never the repository's own, for [CodeSeam.Converse]'s reason. A
// write answered UNCHANGED raises nothing.
func (c CodeSeam) Checks(ctx context.Context, r gitlet.ChecksRequest) (gitlet.ChecksResult, error) {
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
func (c CodeSeam) Resolve(ctx context.Context, q gitlet.ResolveQuery) (gitlet.RepoRef, error) {
	return call(ctx, c.r, opCodeResolve, nil, q)
}

// RepoState answers one page of a repository's snapshot.
func (c CodeSeam) RepoState(ctx context.Context, q gitlet.StateQuery) (gitlet.RepoSnapshot, error) {
	return call(ctx, c.r, opCodeRepoState, nil, q)
}

// PushOutcome answers whether a push this node proposed landed and, where it
// did, raises this node's floors past it, as [CodeSeam.Propose] does.
func (c CodeSeam) PushOutcome(ctx context.Context, q gitlet.OutcomeQuery) (gitlet.PushOutcome, error) {
	out, err := call(ctx, c.r, opCodePushOutcome, nil, q)
	if out.Found {
		c.wrote(q.RepoID, out.Push.Position)
	}
	return out, err
}

// Read answers a read of Gitlet: a person's or a surface's, and the engine's
// own that no principal makes (a cache's `introduced` backfill, a compactor's
// `compaction_due` listing, a collected executor run's `run_pushes`).
func (c CodeSeam) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	return call(ctx, c.r, opCodeRead, nil, q)
}

// QueueDue answers a page of the queue work a repository's home owes, read
// whole or refused ([opCodeQueueDue]): the home's merge queue's read. It
// raises no floor.
//
// The engine's alone: its answer names whom each enqueue was made for
// (gitlet.QueueWorkEntry.WorkingFor), which no reader of a queue may see, so
// the engine-class guard refuses it as it refuses [TurnThreads]. A surface
// lists a queue through [CodeSeam.Read]'s `queue` kind, whose rows leave it
// out.
func (c CodeSeam) QueueDue(ctx context.Context, q gitlet.QueueDueQuery) (gitlet.QueueDueAnswer, error) {
	return call(ctx, c.r, opCodeQueueDue, nil, q)
}

// TurnsMovedCode answers which of a set of turns pushed, opened a change or
// gave a review.
func (c CodeSeam) TurnsMovedCode(ctx context.Context, q gitlet.TurnsMovedQuery) (map[string]bool, error) {
	return call(ctx, c.r, opCodeTurnsMoved, nil, q)
}

// Lifecycle asks a data node to decide one lifecycle request (a tool's create,
// rename, settings change, archive, unarchive, delete, restore or protection,
// or a live turn's hold), and raises this node's floors past the record it
// landed in where it APPLIED or is PENDING. Never for a refusal, nor for an
// answer that needed no record, whose position is somebody else's rows.
func (c CodeSeam) Lifecycle(ctx context.Context, r gitlet.LifecycleRequest) (gitlet.LifecycleResult, error) {
	out, err := call(ctx, c.r, opCodeLifecycle, nil, r)
	wrote := out.Refusal == "" && !out.Position.IsZero() && out.Repo.ID != "" &&
		(out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending)
	if wrote {
		c.wrote(out.Repo.ID, out.Position)
	}
	return out, err
}

// ResolveRef answers one ref of one repository at the level the query names.
// It raises no floor.
func (c CodeSeam) ResolveRef(ctx context.Context, q gitlet.RefQuery) (gitlet.RefAnswer, error) {
	return call(ctx, c.r, opCodeResolveRef, nil, q)
}

// GitRead asks the repository's home Gitlet node for one read of its content by
// object id, and its runner-up when the home cannot answer (ADR-0089). Its
// floor is the query's own (gitlet.GitQuery.Fresh), which [CodeSeam.FloorOf]
// supplies where the reader's own writes must be in the copy it reads.
func (c CodeSeam) GitRead(ctx context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	return call(ctx, c.r, opCodeGitRead, nil, q)
}

// MergePreview asks the repository's home Gitlet node, and its runner-up when
// the home cannot answer (ADR-0089), what merging q.Head into q.Base would do,
// by the merge the home records a change's mergeability by. Its floor is the
// query's own (gitlet.MergePreviewQuery.Fresh), as [CodeSeam.GitRead]'s is. It
// raises no floor: a preview writes nothing.
func (c CodeSeam) MergePreview(ctx context.Context, q gitlet.MergePreviewQuery) (gitlet.MergePreviewAnswer, error) {
	return call(ctx, c.r, opCodeMergePreview, nil, q)
}

// GitWrite asks the repository's home Gitlet node to build one engine write and
// land it as a push (ADR-0092), and its runner-up with the same push where the
// home ran nothing or went silent (ADR-0089). Its floor is the write's own
// (gitlet.EngineWrite.Fresh). A write gitlet.EngineWrite.Validate refuses is
// gitlet.ErrInvalid here, before any Gitlet node spends a round trip on an
// answer every node gives alike; except a value naming whom it is made for
// that this build does not know (gitlet.IsUnknownToThisBuild): Validate meets
// it before any other rule, so only a node that knows it can judge the write,
// malformed or not; it is asked, and answered as the wire does ([unreadHere]).
//
// It raises this node's floors where the write APPLIED or is PENDING, an
// answer from an earlier ask's row included; never on a refusal or UNKNOWN.
func (c CodeSeam) GitWrite(ctx context.Context, w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
	if err := w.Validate(); err != nil && !gitlet.IsUnknownToThisBuild(err) {
		return gitlet.EngineWriteResult{}, err
	}
	out, err := call(ctx, c.r, opCodeGitWrite, nil, w)
	if out.Refusal == "" && (out.Outcome == statelog.OutcomeApplied || out.Outcome == statelog.OutcomePending) {
		c.wrote(w.RepoID, out.Position)
	}
	return out, err
}

// FloorOf is this node's floor on repository repoID: the furthest of its own
// writes to that repository it has been told landed, or the zero position. A
// reader hands it to a read by object id as its Fresh, so the home's copy
// holds this node's own pushes before it is read; a git operation carries no
// floor of the router's ([GitCopy]).
//
// Never the session's floor, which spans the whole code log: a push to one
// repository would put every other repository's home copy below it, and each
// read would run a sync round for nothing.
func (c CodeSeam) FloorOf(repoID string) statelog.Position {
	return c.r.codeWrites.floor(repoID)
}
