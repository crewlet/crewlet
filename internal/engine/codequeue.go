package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	codehome "github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// queueTasks is the merge queue's step 2 (M5-D27): a landed change's work
// item closed AS THE ENTRY'S ENQUEUER, never on the queue's own account — the
// home's queue worker hands it each landed entry that asked it
// (internal/code/home's Tasks).
func (e *Engine) queueTasks() codehome.Tasks { return queueCloser{engine: e} }

// queueCloser closes a landed change's work item as its enqueuer.
//
// THE ENQUEUER'S OWN WRITE, DECIDED AS THEM. The close lands on the tracker
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
// ONLY THE ENGINE'S OWN TRACKER is closed here. A change whose work item is a
// vendor's (a Jira issue, a GitHub issue) is closed by whoever closes it there
// — the engine holds no credential of the enqueuer's on a vendor's tracker,
// and its own on their behalf would put somebody else's name on the close — so
// it is answered as an error naming the item, which the queue logs: the
// change has landed whatever becomes of its item.
type queueCloser struct{ engine *Engine }

// errQueueCloseNotNative is a work item no tracker of this engine holds.
var errQueueCloseNotNative = errors.New("engine: the work item is not on the engine's own tracker, " +
	"so it is closed where it lives")

// CloseWorkItem closes c's work item as c.As, under c.OpID: asked twice, the
// tracker's ledger answers the second with the first's result. An item
// already finished — done, cancelled or closed — is left as it is.
func (q queueCloser) CloseWorkItem(ctx context.Context, c codehome.TaskClose) error {
	item := c.WorkItem
	if item.Backend != types.WorkNative {
		return fmt.Errorf("%w: %s %s", errQueueCloseNotNative, item.Backend, item.ID)
	}
	actor, err := enqueuerActor(c.As)
	if err != nil {
		return err
	}
	halves, ok := q.engine.trackerHalves()
	if !ok {
		return fmt.Errorf("engine: close work item %s: this node writes to no tracker of the engine's own",
			item.ID)
	}
	principal, err := turnRequesters{engine: q.engine}.Requester(ctx, actor.Handle)
	if err != nil {
		return fmt.Errorf("engine: close work item %s as %s: %w", item.ID, actor.Handle, err)
	}
	if err := builtin.DecisionError(authz.ActionWorkUpdate, authz.Decide(ctx, principal,
		authz.ActionWorkUpdate, authz.Object{Kind: authz.KindTask}, ChartAuthorityOf(q.engine),
		time.Now())); err != nil {
		return fmt.Errorf("engine: close work item %s as %s: %w", item.ID, actor.Handle, err)
	}
	// LINEARIZABLE, as every read a seat or an operator decides from: the
	// status read is what says whether the close is still owed.
	before, err := halves.reader.Task(ctx, item.ID, tracker.DetailWants{},
		statelog.Freshness{Level: statelog.DefaultReadLevel(statelog.SurfaceSeat)})
	if err != nil {
		return fmt.Errorf("engine: read work item %s to close it: %w", item.ID, err)
	}
	if g := before.Task.Status.Group(); g == tracker.GroupDone || g == tracker.GroupClosed {
		return nil
	}
	done := tracker.StatusDone
	patch := tracker.TaskPatch{Status: &done}
	got, err := halves.as(actor).UpdateTask(ctx, c.OpID, before.Task.ID, before.Task.Project,
		tracker.NoIfMatch, patch, tracker.ChangeStatus, tracker.Wake{
			Kind: tracker.ChangeStatus, Before: before.Task, After: tracker.Patched(before.Task, patch),
			Excerpt: fmt.Sprintf("Change %d landed through its merge queue.", c.Change.Number),
		}.Notify(liveLeads{engine: q.engine}))
	switch {
	case err != nil:
		return fmt.Errorf("engine: close work item %s as %s: %w", item.ID, actor.Handle, err)
	case got.Outcome == statelog.OutcomeUnknown:
		return fmt.Errorf("engine: close work item %s as %s: nobody can say the close landed; the item's "+
			"history is what says", item.ID, actor.Handle)
	}
	return nil
}

// enqueuerActor is who a queued merge's step 2 writes as: a seat as itself, a
// person as the human seat they hold with the credential they enqueued
// through beside it. A credential bound to no seat enqueues nothing (a merge
// is a bound principal's act), so one here is refused rather than given a
// name of its own.
func enqueuerActor(p code.Principal) (builtin.Actor, error) {
	switch {
	case p.Kind == code.PrincipalAgent && p.Seat != "":
		return builtin.Actor{Handle: p.Seat, Kind: tracker.AuthorAgent}, nil
	case p.Kind == code.PrincipalOperator && p.Seat != "":
		return builtin.Actor{Handle: p.Seat, Kind: tracker.AuthorHuman, OperatorID: p.OperatorID}, nil
	}
	return builtin.Actor{}, fmt.Errorf("engine: a %s principal bound to no seat (%q) closes no work item in "+
		"its own name", p.Kind, p.OperatorID)
}
