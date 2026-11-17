package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	gitlethome "github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// queueTasks is the merge queue's step 2 (M5-D27): a landed change's work
// item closed as the entry's enqueuer, never on the queue's own account
// (internal/gitlet/home's Tasks).
func (e *Engine) queueTasks() gitlethome.Tasks { return queueCloser{engine: e} }

// queueCloser closes a landed change's work item as its enqueuer: a seat by
// its handle, a person as their human seat with their enqueuing credential
// beside it (the inverse of [builtin.CodeAskerOf]), composed as a turn's
// requester ([turnRequesters]). Who may close is decided at the landing, since
// one decided at the enqueue is stale: [mayClose], and for a seat's enqueue in
// a turn somebody woke, within the apps of whom it was made for, composed again
// on this node and written for them (ADR-0109, [types.ForTurn]). Only Worklet
// is closed: the engine holds no credential of the enqueuer's on a vendor's
// tracker, and its own would put another name on the close. A refusal asking
// again cannot change is [gitlethome.NotClosed], told to the change's authors,
// enqueuer and any refused requester (internal/gitlet's close_settled); any
// other failure is a plain error, leaving the close owed for the next turn.
type queueCloser struct{ engine *Engine }

// errQueueCloseNotWorklet is a work item Worklet does not hold.
var errQueueCloseNotWorklet = errors.New("engine: the work item is not on Worklet, " +
	"so it is closed where it lives")

// CloseWorkItem closes c's work item as c.As, for c.WorkingFor, under
// c.OpID: asked twice, the tracker's ledger answers the second with the
// first's result. An item already finished — done, cancelled or closed — is
// left as it is.
func (q queueCloser) CloseWorkItem(ctx context.Context, c gitlethome.TaskClose) error {
	item := c.WorkItem
	// A REQUESTER THIS BUILD CANNOT JUDGE, BEFORE ANY OTHER RULE: a kind only
	// a newer build composes (internal/gitlet's storedCarrier keeps it as
	// written). Not even NotClosed, which that build might answer otherwise:
	// the close stays owed (gitlet.KindUnknownToThisBuild), and the home logs
	// it at WARN each turn until a build that knows the kind takes it.
	if gitlet.KindUnknownToThisBuild(c.WorkingFor) {
		return fmt.Errorf("engine: close work item %s: the fleet's builds differ: whom its enqueue was made "+
			"for is named by a kind this build does not know (%q), so the close stays owed for a home of "+
			"the build that knows it", item.ID, c.WorkingFor.Kind)
	}
	if item.Backend != types.WorkWorklet {
		return &gitlethome.NotClosed{Why: fmt.Sprintf("%s is a %s work item, which is closed where it lives",
			workItemName(item), item.Backend), Err: errQueueCloseNotWorklet}
	}
	// DECIDED BEFORE THIS NODE'S TRACKER IS ASKED FOR, off the chart and the
	// directory: a close that will never be made is settled by whichever home
	// decides it, and only one that may be made waits for a node with a tracker.
	actor, err := decideClose(ctx, turnRequesters(q), ChartAuthorityOf(q.engine), c, time.Now())
	if err != nil {
		return err
	}
	halves, ok := q.engine.trackerHalves()
	if !ok {
		return fmt.Errorf("engine: close work item %s: this node writes to no tracker of the engine's own",
			item.ID)
	}
	// LINEARIZABLE, as every read a seat or an operator decides from: this
	// status says whether the close is still owed.
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

// decideClose is who closes c's work item and whether they may now: the
// enqueuer written for whom the enqueue was made for ([enqueuerActor] of c.As
// and the row's carrier c.WorkingFor), held to [mayClose] as that very actor,
// so the close is decided for the principal it is written as. A principal
// bound to no seat is NotClosed.
func decideClose(ctx context.Context, r builtin.Requesters, chart authz.Chart, c gitlethome.TaskClose,
	now time.Time) (builtin.Actor, error) {

	actor, err := enqueuerActor(c.As, c.WorkingFor)
	if err != nil {
		return builtin.Actor{}, &gitlethome.NotClosed{Why: "whoever queued the change acts as no seat, and " +
			"nobody else closes the item in their name", Err: err}
	}
	if err := mayClose(ctx, r, chart, c.WorkItem, actor, now); err != nil {
		return builtin.Actor{}, err
	}
	return actor, nil
}

// mayClose is whether actor ([enqueuerActor]) may close item now, decided on
// this node from r's composition and chart: nil where it may; a
// [gitlethome.NotClosed] naming who may not and why, where asking again
// changes nothing; any other error where this node cannot say yet, which
// leaves the close owed.
//
// The enqueuer's own authority first (all a close made for nobody is decided
// by), then, where the actor writes for somebody, the ceiling their carrier
// composes on this node ([builtin.WorkingForCeiling],
// [authz.DecideWithinApps]): what a call of the enqueuing turn would get now.
// A seat the organisation no longer holds is the composer's definitive answer
// ([errNoSuchSeat]), told apart from a read that failed.
func mayClose(ctx context.Context, r builtin.Requesters, chart authz.Chart, item types.WorkItem,
	actor builtin.Actor, now time.Time) error {

	principal, err := r.Requester(ctx, actor.Handle)
	switch {
	case errors.Is(err, errNoSuchSeat):
		return &gitlethome.NotClosed{Why: fmt.Sprintf("%s, who queued the change, holds no seat in the "+
			"organisation any more", actor.Handle), Err: err}
	case err != nil:
		return fmt.Errorf("engine: close work item %s as %s: %w", item.ID, actor.Handle, err)
	}
	ceiling, whom := authz.Ceiling{Kind: authz.CeilingNone}, ""
	if carrier := actor.WrittenFor.Carrier(); carrier != nil {
		whom = requesterNamed(*carrier)
		ceiling, err = builtin.WorkingForCeiling(ctx, *carrier, actor.Handle, r)
		switch {
		case errors.Is(err, errNoSuchSeat):
			return &gitlethome.NotClosed{Why: fmt.Sprintf("%s, for whom %s queued the change, holds no seat in "+
				"the organisation any more", whom, actor.Handle), Err: err}
		case err != nil:
			return fmt.Errorf("engine: close work item %s as %s for %s: %w", item.ID, actor.Handle, whom, err)
		}
	}
	err = builtin.DecisionError(authz.ActionWorkUpdate, authz.DecideWithinApps(ctx, principal, ceiling,
		authz.ActionWorkUpdate, authz.Object{Kind: authz.KindTask}, chart, now))
	var refused *builtin.DecisionRefused
	switch {
	case errors.As(err, &refused) && refused.Decision.Reason == authz.ReasonCeiling:
		return &gitlethome.NotClosed{Why: refusedWithin(whom, actor.Handle, refused.Decision), Err: err}
	case errors.As(err, &refused):
		return &gitlethome.NotClosed{Why: fmt.Sprintf("%s, who queued the change, may not update work "+
			"items any more (%s)", actor.Handle, refused.Decision.Reason), Err: err}
	case err != nil:
		return fmt.Errorf("engine: close work item %s as %s: %w", item.ID, actor.Handle, err)
	}
	return nil
}

// refusedWithin is the sentence the change's authors and enqueuer read beside
// `merged` for a close refused within the apps of whom, for whom enqueuer
// queued it. A missing app is named, since an administrator granting it is
// what changes the answer.
func refusedWithin(whom, enqueuer string, d authz.Decision) string {
	switch {
	case d.Ceiling == authz.CeilingStranger:
		return fmt.Sprintf("%s queued the change for somebody the company does not know (%s), who uses no "+
			"app, so the item is left open", enqueuer, whom)
	case d.RequesterReason == authz.ReasonStage:
		return fmt.Sprintf("%s, for whom %s queued the change, may not act in the company now, so the item "+
			"is left open", whom, enqueuer)
	case len(d.RequesterGrants) > 0:
		apps := make([]words.Said, 0, len(d.RequesterGrants))
		for _, app := range d.RequesterGrants {
			name := string(app)
			if name != "" {
				name = strings.ToUpper(name[:1]) + name[1:]
			}
			apps = append(apps, words.Said(name))
		}
		return fmt.Sprintf("%s, for whom %s queued the change, does not use %s, which closing the item "+
			"needs, so it is left open", whom, enqueuer, words.Or(apps))
	}
	return fmt.Sprintf("%s, for whom %s queued the change, could not update the item themselves, so it is "+
		"left open", whom, enqueuer)
}

// requesterNamed is how a sentence names whom a write was made for: a seat by
// its handle, a credential by its login, and anybody else by their kind.
func requesterNamed(w types.WorkingFor) string {
	switch {
	case w.Kind == types.WorkingForSeat && w.Seat != "":
		return w.Seat
	case w.Kind == types.WorkingForCredential && w.Login != "":
		return w.Login
	}
	return "a " + string(w.Kind)
}

// closeFailed is how a tracker's failure to read or close item as handle
// reads to the queue. A refusal Worklet marks final — the item gone
// ([worklet.ErrNoTask]; the read is linearizable, so gone, not unapplied), not
// the enqueuer's to write ([worklet.ErrForbidden]), or a close its content or
// state refuses ([worklet.ErrInvalid], [worklet.ErrState]) — is
// [gitlethome.NotClosed], so its people hear it once rather than the queue
// asking every turn; any other failure is the node's own, asked again.
//
// Why is the engine's own sentence per mark, never Worklet's refusal text,
// which is worded for one reader while this reaches a person's Inbox and a
// seat's wake (ADR-0060); Err keeps the refusal for the log.
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

// enqueuerActor is who a queued merge's step 2 writes as: a seat as itself,
// written for whom its enqueue was made for ([types.ForTurn] of workingFor,
// nil for nobody), and a person as their human seat with their credential
// beside it, writing for nobody but themselves. A credential bound to no seat
// cannot have enqueued (a merge is a bound principal's act), so one is refused
// rather than given a name of its own.
func enqueuerActor(p gitlet.Principal, workingFor *types.WorkingFor) (builtin.Actor, error) {
	switch {
	case p.Kind == gitlet.PrincipalAgent && p.Seat != "":
		return builtin.Actor{Handle: p.Seat, Kind: worklet.AuthorAgent, WrittenFor: types.ForTurn(workingFor)}, nil
	case p.Kind == gitlet.PrincipalOperator && p.Seat != "":
		return builtin.Actor{Handle: p.Seat, Kind: worklet.AuthorHuman, OperatorID: p.OperatorID}, nil
	}
	return builtin.Actor{}, fmt.Errorf("engine: a %s principal bound to no seat (%q) closes no work item in "+
		"its own name", p.Kind, p.OperatorID)
}
