package estate

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ---- the tracker's writes ------------------------------------------------ //

// A PROJECT'S FILES, as rows. The BYTES never cross here: every node streams
// them to and from the fleet's one object store through its own client, and
// asks this service only for the row that names them.
var opFiles = define("tracker.files", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, q worklet.FileQuery) (worklet.FileListing, error) {
		if b.Tracker == nil {
			return worklet.FileListing{}, errNoHalf
		}
		return b.Tracker.Files(ctx, q)
	})

type fileArgs struct {
	Project string
	Path    string
	Fresh   statelog.Freshness
}

var opFile = define("tracker.read_file", opRead, trackerDomain, false,
	func(ctx context.Context, b Backend, _ *Actor, a fileArgs) (worklet.FileDetail, error) {
		if b.Tracker == nil {
			return worklet.FileDetail{}, errNoHalf
		}
		return b.Tracker.File(ctx, a.Project, a.Path, a.Fresh)
	})

type putFileArgs struct {
	OpID string
	Put  worklet.FilePut
}

var opPutFile = define("tracker.write_file", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a putFileArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
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
	func(ctx context.Context, b Backend, actor *Actor, a removeFileArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.RemoveFile(ctx, a.OpID, a.Project, a.Path, a.IfMatch)
	})

type recordTurnArgs struct {
	OpID string
	Turn worklet.TurnRecord
}

// A TURN'S SPEND, recorded by the node that ran the turn, which may hold no
// applier. Its operation id makes asking the next node again safe: the
// applier adds a turn's spend only for a new operation.
var opRecordTurn = define("tracker.record_turn", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a recordTurnArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
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
	Task   worklet.Task
	Notify *worklet.Notify
}

// A TASK HOLDING THE ZERO INSTANT is refused before it is asked
// ([op.refusedBeforeAsking]): a pointer at it would cross as no instant at all,
// and the writer refuses it where it runs ([worklet.Task.CheckInstants]).
var opCreateTask = define("tracker.create_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a createTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.CreateTask(ctx, a.OpID, a.Task, a.Notify)
	}).refusedBeforeAsking(func(a createTaskArgs) error { return a.Task.CheckInstants() })

type createTaskAskingArgs struct {
	OpID   string
	Task   worklet.Task
	Ask    worklet.Comment
	Notify *worklet.Notify
}

// The task and its ask are refused before they are asked for the zero instant
// as a create's task is ([opCreateTask]).
var opCreateTaskAsking = define("tracker.create_task_asking", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a createTaskAskingArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.CreateTaskAsking(ctx, a.OpID, a.Task, a.Ask, a.Notify)
	}).refusedBeforeAsking(func(a createTaskAskingArgs) error {
	if err := a.Task.CheckInstants(); err != nil {
		return err
	}
	return a.Ask.CheckInstants()
})

type placeTaskArgs struct {
	OpID   string
	Place  worklet.Place
	Notify *worklet.Notify
}

// placedTask is [TrackerWriter.PlaceTask]'s answer as it crosses: the result,
// with the reason the card changed lanes and did not take its place carried
// BESIDE it as a wire error — an error is an interface no encoder writes, and
// the tool that reads it classifies the refusal by its identity (a stale
// version, a conflict), which [encodeError] keeps.
type placedTask struct {
	Result   worklet.PlaceResult
	Unplaced *wireError
}

// A DROP ON THE BOARD WALKS one task (its lane, then its place among its
// neighbours), each step under an operation id derived from the caller's, so
// the whole is asked again of the next holder and a step its ledger answers is
// not written twice. A step answered unvouched is not final ([unvouched]).
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
	Patch     worklet.TaskPatch
	Watch     *worklet.WatchIntent
	Relate    *worklet.RelationIntent
	Depend    *worklet.DependentIntent
	Checklist *worklet.ChecklistIntent
	Kind      worklet.ChangeKind
	Notify    *worklet.Notify
}

// A PATCH'S DATES are refused before they are asked where no task could hold
// them ([op.refusedBeforeAsking], [worklet.TaskPatch.CheckDates]): a date set to
// the zero instant would cross as no date named at all — a clear is its own
// flag, ClearStart or ClearDue, which crosses as itself.
var opUpdateTask = define("tracker.update_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a updateTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		patch := a.Patch
		patch.Watch, patch.Relate, patch.Depend, patch.Checklist =
			a.Watch, a.Relate, a.Depend, a.Checklist
		return w.UpdateTask(ctx, a.OpID, a.ID, a.Project, a.IfMatch, patch, a.Kind, a.Notify)
	}).refusedBeforeAsking(func(a updateTaskArgs) error { return a.Patch.CheckDates() })

type dependArgs struct {
	OpID   string
	Change worklet.DependencyChange
}

var opDepend = define("tracker.depend", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a dependArgs) (worklet.DependencyResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.DependencyResult{}, err
		}
		return w.Depend(ctx, a.OpID, a.Change, b.Leads)
	})

type mergeArgs struct {
	OpID      string
	Duplicate string
	Into      string
	Reparent  bool
	Notify    *worklet.Notify
}

var opMergeDuplicates = define("tracker.merge_duplicates", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a mergeArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.MergeDuplicates(ctx, a.OpID, a.Duplicate, a.Into, a.Reparent, a.Notify)
	})

type moveArgs struct {
	OpID   string
	TaskID string
	Target string
	Notify *worklet.Notify
}

var opMoveTaskToProject = define("tracker.move_task_to_project", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a moveArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.MoveTaskToProject(ctx, a.OpID, a.TaskID, a.Target, a.Notify)
	})

type writeProjectArgs struct {
	OpID      string
	Key       string
	Edit      worklet.ProjectEdit
	Authority worklet.ProjectAuthority
}

var opWriteProject = define("tracker.write_project", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeProjectArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.WriteProject(ctx, a.OpID, a.Key, a.Edit, a.Authority)
	})

type writeTagsArgs struct {
	OpID      string
	Project   string
	Edit      worklet.TagEdit
	Authority worklet.TagAuthority
}

var opWriteTags = define("tracker.write_tags", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeTagsArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.WriteTags(ctx, a.OpID, a.Project, a.Edit, a.Authority)
	})

type ensureTagsArgs struct {
	OpID    string
	Project string
	Tags    []string
}

// ensuredTags is [TrackerWriter.EnsureTags]' two answers, named. Its caveats
// are rebuilt from the peer's answer at the router, as a write's are
// ([worklet.WriteResult.Caveats]).
type ensuredTags struct {
	Created []string
	Caveats []caveat.Caveat
}

var opEnsureTags = define("tracker.ensure_tags", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a ensureTagsArgs) (ensuredTags, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return ensuredTags{}, err
		}
		created, caveats, err := w.EnsureTags(ctx, a.OpID, a.Project, a.Tags)
		return ensuredTags{Created: created, Caveats: caveats}, err
	})

// ---- the operator's writes ------------------------------------------------ //

// The writes only the operator's surfaces make — no seat is handed a tool that
// reaches them — routed for the reason every other write is: the node the
// operator asked may hold no data, or a copy that is out of service.

type viewPriorArgs struct {
	ID string
}

// WHAT A SAVE WOULD REPLACE, read for the caller to decide its authority on and
// hand back to the save. A READ, floored like every tracker read; the save
// refuses a prior that has moved in its own snapshot, so a stale answer here
// is a refusal there, never a wrong overwrite. ANSWERED WHOLE
// ([op.answeredWhole]) because the save hands it back: a newer build's field
// dropped here would reach that build's decide as its zero.
var opViewPrior = define("tracker.view_prior", opRead, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a viewPriorArgs) (worklet.ViewPrior, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.ViewPrior{}, err
		}
		return w.ViewPrior(ctx, a.ID)
	}).answeredWhole()

type writeViewArgs struct {
	OpID  string
	View  worklet.View
	Prior worklet.ViewPrior
}

var opWriteView = define("tracker.write_view", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeViewArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.WriteView(ctx, a.OpID, a.View, a.Prior)
	})

type deleteViewArgs struct {
	OpID  string
	ID    string
	Prior worklet.ViewPrior
}

// A VIEW TAKEN AWAY, decided on the prior its caller read as a save is, and
// refused where the stored view moved, changed hands or went away since.
var opDeleteView = define("tracker.delete_view", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a deleteViewArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.DeleteView(ctx, a.OpID, a.ID, a.Prior)
	})

type writeTypesArgs struct {
	OpID  string
	Types []worklet.TaskType
}

var opWriteTypes = define("tracker.write_types", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeTypesArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.WriteTypes(ctx, a.OpID, a.Types)
	})

type writeFieldsArgs struct {
	OpID   string
	Fields []worklet.FieldDef
}

var opWriteFields = define("tracker.write_fields", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writeFieldsArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.WriteFields(ctx, a.OpID, a.Fields)
	})

type markInboxArgs struct {
	OpID   string
	Handle string
	// Seen is how far the asker read the Worklet's log, which the decide
	// waits for ([worklet.Writer.MarkInbox]). ADDITIVE ON THE WIRE: an older
	// peer decodes past it, and an older sender's zero position waits for
	// nothing.
	Seen      statelog.Position
	Gesture   inbox.Gesture[worklet.Reason]
	Authority worklet.PersonAuthority
}

// A GESTURE, never the lists: the tracker resolves it against the record its
// own decide reads, on whichever holder answers, so a caller never sends back
// a list read in another transaction. The CALLER'S FLOOR rides with it, not in
// this node's session: it is one person's page's claim, held to this request.
var opMarkInbox = define("tracker.mark_inbox", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a markInboxArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.MarkInbox(ctx, a.OpID, a.Handle, a.Seen, a.Gesture, a.Authority)
	})

type writePinsArgs struct {
	OpID      string
	Handle    string
	Gesture   worklet.PinGesture
	Authority worklet.PersonAuthority
}

var opWritePins = define("tracker.write_pins", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writePinsArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.WritePins(ctx, a.OpID, a.Handle, a.Gesture, a.Authority)
	})

type writePrioritiesArgs struct {
	OpID       string
	Handle     string
	Priorities []string
	IfMatch    *uint64
	Authority  worklet.PersonAuthority
}

var opWritePriorities = define("tracker.write_priorities", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a writePrioritiesArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.WritePriorities(ctx, a.OpID, a.Handle, a.Priorities, a.IfMatch, a.Authority)
	})

type editCommentOnTaskArgs struct {
	OpID      string
	TaskID    string
	Project   string
	CommentID string
	Body      string
	Notify    *worklet.Notify
}

// REWRITING ONE'S OWN REMARK on a work item. Its author is decided in the
// decide's own snapshot on the serving node ([worklet.ErrNotAuthor]), against
// the actor this request names — which is why the actor is the SIGNED one
// (see [Envelopes]): an edit whose author a request could choose would put
// words in somebody else's mouth.
var opEditTaskComment = define("tracker.edit_comment", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a editCommentOnTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.EditComment(ctx, a.OpID, a.TaskID, a.Project, a.CommentID, a.Body, a.Notify)
	})

type resolveCommentOnTaskArgs struct {
	OpID      string
	TaskID    string
	Project   string
	CommentID string
	// Withdrawal is why its asker withdraws the question; nil resolves a
	// remark, which is what an older build does.
	Withdrawal *worklet.Withdrawal
	Notify     *worklet.Notify
}

// RESOLVING ONE'S OWN REMARK, a question its asker withdraws: the author is
// decided against the SIGNED actor ([worklet.ErrNotAuthor]), as an edit's is.
var opResolveTaskComment = define("tracker.resolve_comment", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a resolveCommentOnTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.ResolveComment(ctx, a.OpID, a.TaskID, a.Project, a.CommentID, a.Withdrawal, a.Notify)
	})

type removeCommentOnTaskArgs struct {
	OpID      string
	TaskID    string
	Project   string
	CommentID string
	Authority worklet.CommentAuthority
}

// TAKING A REMARK DOWN on a work item: its author, or a moderator the caller's
// table admitted, decided against the SIGNED actor ([worklet.ErrNotAuthor]),
// as an edit's author is.
var opRemoveTaskComment = define("tracker.remove_comment", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a removeCommentOnTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.RemoveComment(ctx, a.OpID, a.TaskID, a.Project, a.CommentID, a.Authority)
	})

type removeTaskArgs struct {
	OpID    string
	ID      string
	Project string
	Subtree bool
	Notify  *worklet.Notify
}

var opRemoveTask = define("tracker.remove_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a removeTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.RemoveTask(ctx, a.OpID, a.ID, a.Project, a.Subtree, a.Notify)
	})

type restoreTaskArgs struct {
	OpID    string
	ID      string
	Project string
	Alone   bool
	Notify  *worklet.Notify
}

var opRestoreTask = define("tracker.restore_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a restoreTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.RestoreTask(ctx, a.OpID, a.ID, a.Project, a.Alone, a.Notify)
	})

type purgeTaskArgs struct {
	OpID    string
	ID      string
	Project string
	Reason  string
}

// A PURGE's operation id is what a repeat after an unanswered one collapses
// on, its ledger row answering with the first outcome; its refusals keep their
// identity, so an operator re-running one is told it is purged, not behind.
var opPurgeTask = define("tracker.purge_task", opIdempotentWrite, trackerDomain, true,
	func(ctx context.Context, b Backend, actor *Actor, a purgeTaskArgs) (worklet.WriteResult, error) {
		w, err := writerFor(b, actor)
		if err != nil {
			return worklet.WriteResult{}, err
		}
		return w.PurgeTask(ctx, a.OpID, a.ID, a.Project, a.Reason)
	})
