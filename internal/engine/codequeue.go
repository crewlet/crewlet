package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	gitlethome "github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// queueTasks is the merge queue's step 2 (M5-D27): a landed change's work
// item closed AS THE ENTRY'S ENQUEUER, never on the queue's own account — the
// home's queue worker hands it each landed entry that asked it
// (internal/gitlet/home's Tasks).
func (e *Engine) queueTasks() gitlethome.Tasks { return queueCloser{engine: e} }

// queueCloser closes a landed change's work item as its enqueuer.
//
// THE ENQUEUER'S OWN WRITE, DECIDED AS THEM. The close lands on Worklet
// through the writer every seat's tool and every person's route writes
// through, attributed to the enqueuer — a seat by its handle, a person by the
// seat they hold with the credential they enqueued through beside it, the
// actor [builtin.CodeAskerOf] reads the other way — and the authority table is
// asked `update_work_item` for them first, on the principal a turn's requester
// is composed as ([turnRequesters]): a seat that left the chart, or a person
// removed since they enqueued, closes nothing. That principal holds every app
// and never a person's own grants, which are decided where a session is
// composed and nowhere else; the enqueue itself was that decision, made when
// the person asked their merge to close the item.
//
// ONLY WORKLET is closed here. A change whose work item is a
// vendor's (a Jira issue, a GitHub issue) is closed by whoever closes it there
// — the engine holds no credential of the enqueuer's on a vendor's tracker,
// and its own on their behalf would put somebody else's name on the close — so
// it is answered NOT CLOSED ([gitlethome.NotClosed]) naming the item, which
// the queue settles and tells the change's authors and its enqueuer: the
// change has landed whatever becomes of its item.
//
// WHAT IS FINAL AND WHAT IS NOT: a vendor's item, an enqueuer bound to no
// seat, a seat the chart no longer holds, an authority refusal and a close
// Worklet itself refuses by its marks ([closeFailed]: the item gone, not the
// enqueuer's to write, or its content or its state refusing the close) are
// answered NotClosed, since asking again changes none of them; every other
// failure — no tracker on this node, a read or a write that failed, an
// outcome nobody can state — is a plain error, which leaves the close owed
// for the queue's next turn on whichever home.
type queueCloser struct{ engine *Engine }

// errQueueCloseNotWorklet is a work item Worklet does not hold.
var errQueueCloseNotWorklet = errors.New("engine: the work item is not on Worklet, " +
	"so it is closed where it lives")

// CloseWorkItem closes c's work item as c.As, under c.OpID: asked twice, the
// tracker's ledger answers the second with the first's result. An item
// already finished — done, cancelled or closed — is left as it is.
func (q queueCloser) CloseWorkItem(ctx context.Context, c gitlethome.TaskClose) error {
	item := c.WorkItem
	if item.Backend != types.WorkWorklet {
		return &gitlethome.NotClosed{Why: fmt.Sprintf("%s is a %s work item, which is closed where it lives",
			workItemName(item), item.Backend), Err: errQueueCloseNotWorklet}
	}
	actor, err := enqueuerActor(c.As)
	if err != nil {
		return &gitlethome.NotClosed{Why: "whoever queued the change acts as no seat, and nobody else closes " +
			"the item in their name", Err: err}
	}
	halves, ok := q.engine.trackerHalves()
	if !ok {
		return fmt.Errorf("engine: close work item %s: this node writes to no tracker of the engine's own",
			item.ID)
	}
	principal, err := turnRequesters(q).Requester(ctx, actor.Handle)
	switch {
	case errors.Is(err, errNoSuchSeat):
		return &gitlethome.NotClosed{Why: fmt.Sprintf("%s, who queued the change, holds no seat in the "+
			"organisation any more", actor.Handle), Err: err}
	case err != nil:
		return fmt.Errorf("engine: close work item %s as %s: %w", item.ID, actor.Handle, err)
	}
	err = builtin.DecisionError(authz.ActionWorkUpdate, authz.Decide(ctx, principal,
		authz.ActionWorkUpdate, authz.Object{Kind: authz.KindTask}, ChartAuthorityOf(q.engine), time.Now()))
	var refused *builtin.DecisionRefused
	switch {
	case errors.As(err, &refused):
		return &gitlethome.NotClosed{Why: fmt.Sprintf("%s, who queued the change, may not update work "+
			"items any more (%s)", actor.Handle, refused.Decision.Reason), Err: err}
	case err != nil:
		return fmt.Errorf("engine: close work item %s as %s: %w", item.ID, actor.Handle, err)
	}
	// LINEARIZABLE, as every read a seat or an operator decides from: the
	// status read is what says whether the close is still owed.
	before, err := halves.reader.Task(ctx, item.ID, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)})
	if err != nil {
		return closeFailed(item, actor.Handle, "read", err)
	}
	if g := before.Task.Status.Group(); g == worklet.GroupDone || g == worklet.GroupClosed {
		return nil
	}
	done := worklet.StatusDone
	patch := worklet.TaskPatch{Status: &done}
	got, err := halves.as(actor).UpdateTask(ctx, c.OpID, before.Task.ID, before.Task.Project,
		worklet.NoIfMatch, patch, worklet.ChangeStatus, worklet.Wake{
			Kind: worklet.ChangeStatus, Before: before.Task, After: worklet.Patched(before.Task, patch),
			Excerpt: fmt.Sprintf("Change %d landed through its merge queue.", c.Change.Number),
		}.Notify(liveLeads(q)))
	switch {
	case err != nil:
		return closeFailed(item, actor.Handle, "close", err)
	case got.Outcome == statelog.OutcomeUnknown:
		return fmt.Errorf("engine: close work item %s as %s: nobody can say the close landed; the item's "+
			"history is what says", item.ID, actor.Handle)
	}
	return nil
}

// closeFailed is how a tracker's failure to read or close item as handle
// reads to the queue. A refusal Worklet MARKS as one no asking again changes
// — the item gone ([worklet.ErrNoTask]: the read is linearizable, so it is
// gone, not unapplied), not the enqueuer's to write ([worklet.ErrForbidden]),
// a close its content or its state refuses ([worklet.ErrInvalid],
// [worklet.ErrState]: a project's rule for done, an item in the trash) — is
// [gitlethome.NotClosed], so its people hear it once rather than the queue
// asking it every turn for the queue's whole history; every other failure is
// the node's own, which the queue asks again.
//
// THE ENGINE'S OWN SENTENCE, one a mark, never Worklet's refusal text: the
// sentence is read by a person in their Inbox and by a seat in its wake, and
// a refusal's words are worded for one reader (ADR-0060). Err keeps the
// refusal for the log.
func closeFailed(item types.WorkItem, handle, doing string, err error) error {
	name := workItemName(item)
	var why string
	switch {
	case errors.Is(err, worklet.ErrNoTask):
		why = fmt.Sprintf("%s is no longer on Worklet", name)
	case errors.Is(err, worklet.ErrForbidden):
		why = fmt.Sprintf("Worklet does not let %s, who queued the change, update %s", handle, name)
	case errors.Is(err, worklet.ErrInvalid):
		why = fmt.Sprintf("Worklet refuses to set %s done as it stands", name)
	case errors.Is(err, worklet.ErrState):
		why = fmt.Sprintf("%s cannot be closed in the state it is in, such as in the trash", name)
	default:
		return fmt.Errorf("engine: %s work item %s as %s: %w", doing, item.ID, handle, err)
	}
	return &gitlethome.NotClosed{Why: why, Err: err}
}

// workItemName is how a person names item: its key where it has one (ENG-42),
// its id otherwise.
func workItemName(item types.WorkItem) string {
	if item.Key != "" {
		return item.Key
	}
	return item.ID
}

// enqueuerActor is who a queued merge's step 2 writes as: a seat as itself, a
// person as the human seat they hold with the credential they enqueued
// through beside it. A credential bound to no seat enqueues nothing (a merge
// is a bound principal's act), so one here is refused rather than given a
// name of its own.
//
// A SEAT'S CLOSE IS MADE FOR NOBODY ELSE ([types.ForNobody]): the queue lands
// the change after the turn that enqueued it ended, outside any turn, and a
// queued change carries nobody it was made for, so the turn this close wakes
// works for the seat. That is Gitlet's edge of ADR-0109 until crewlet#798's
// piece 4c carries a carrier on code's records, and this close with it.
func enqueuerActor(p gitlet.Principal) (builtin.Actor, error) {
	switch {
	case p.Kind == gitlet.PrincipalAgent && p.Seat != "":
		return builtin.Actor{Handle: p.Seat, Kind: worklet.AuthorAgent, WrittenFor: types.ForNobody()}, nil
	case p.Kind == gitlet.PrincipalOperator && p.Seat != "":
		return builtin.Actor{Handle: p.Seat, Kind: worklet.AuthorHuman, OperatorID: p.OperatorID}, nil
	}
	return builtin.Actor{}, fmt.Errorf("engine: a %s principal bound to no seat (%q) closes no work item in "+
		"its own name", p.Kind, p.OperatorID)
}
