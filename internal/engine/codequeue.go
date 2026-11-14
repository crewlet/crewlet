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
// removed since they enqueued, closes nothing, and a person is held to the
// apps their own grants name now, as every write of theirs is. The enqueue
// decided nothing of Worklet, and a decision made then would be stale at the
// landing.
//
// AND FOR WHOM THE ENQUEUE WAS MADE (ADR-0109). A seat's enqueue made in a
// turn somebody woke is that turn's write, and the close is the same turn's
// write made late, after the turn ended, as a run's change its collection
// opens is: written for them ([types.ForTurn] of the entry's carrier,
// [gitlethome.TaskClose.WorkingFor]), and decided within what they may use —
// the ceiling the carrier composes AGAIN on this node, by the one composition
// every call of a turn is decided under ([builtin.WorkingForCeiling],
// [authz.DecideWithinApps]) — so a revocation since the enqueue applies, a
// credential's apps are narrowed again by this node's resolution, and a person
// without Worklet who had a seat merge a change into a queue does not have its
// item closed for them, as the seat's direct merge in their turn would not
// close it either. A requester the organisation no longer holds, or one who
// may not use Worklet, is answered NOT CLOSED, naming them, which the change's
// authors and its enqueuer hear, for that requester too (internal/gitlet's
// close_settled routing). A close made for nobody — the seat's own work, a
// person's enqueue, an entry from before the carrier — is decided for the
// enqueuer alone, as before the carrier.
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
// seat, a seat the chart no longer holds — the enqueuer's, or the requester's
// whom the enqueue was made for — an authority refusal, for the enqueuer or
// within the requester's apps, and a close Worklet itself refuses by its
// marks ([closeFailed]: the item gone, not the enqueuer's to write, or its
// content or its state refusing the close) are answered NotClosed, since
// asking again changes none of them; every other failure — no tracker on this
// node, a read or a write that failed, a requester this node could not
// compose, an outcome nobody can state — is a plain error, which leaves the
// close owed for the queue's next turn on whichever home. So is a requester
// named by a kind this build does not know, which this build cannot judge at
// all ([gitlet.KindUnknownToThisBuild]): the close is deferred, never decided,
// until a home of the build that knows the kind takes it.
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
	// A REQUESTER THIS BUILD CANNOT JUDGE, BEFORE ANY OTHER RULE: whom the
	// enqueue was made for is named by a kind only a newer build composes,
	// as the entry's row holds it (internal/gitlet's storedCarrier, which
	// never rewrites one). This build decides nothing of such a close — not
	// even that it will never be made, which a home of the build that knows
	// the kind might answer otherwise — so it stays owed, as the one rule
	// answers such a carrier everywhere (gitlet.KindUnknownToThisBuild), and
	// the home logs why at WARN on every turn until one of that build takes
	// the repository.
	if gitlet.KindUnknownToThisBuild(c.WorkingFor) {
		return fmt.Errorf("engine: close work item %s: the fleet's builds differ: whom its enqueue was made "+
			"for is named by a kind this build does not know (%q), so the close stays owed for a home of "+
			"the build that knows it", item.ID, c.WorkingFor.Kind)
	}
	if item.Backend != types.WorkWorklet {
		return &gitlethome.NotClosed{Why: fmt.Sprintf("%s is a %s work item, which is closed where it lives",
			workItemName(item), item.Backend), Err: errQueueCloseNotWorklet}
	}
	// DECIDED BEFORE THIS NODE'S TRACKER IS ASKED FOR: who closes the item,
	// and whether they may, is read off the company's chart and directory,
	// never off this node's tracker, so a close that will never be made is
	// settled by whichever home decides it, and only one that may be made
	// waits for a node that writes to a tracker.
	actor, err := decideClose(ctx, turnRequesters(q), ChartAuthorityOf(q.engine), c, time.Now())
	if err != nil {
		return err
	}
	halves, ok := q.engine.trackerHalves()
	if !ok {
		return fmt.Errorf("engine: close work item %s: this node writes to no tracker of the engine's own",
			item.ID)
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

// decideClose is who closes c's work item and whether they may now, decided
// on this node from r's composition of them and chart: the entry's enqueuer,
// written for whom its enqueue was made for ([enqueuerActor] of c.As and
// c.WorkingFor, the carrier the home read off the entry's row), and
// [mayClose] of that very actor — so the close is decided for the principal
// it is then written as, never for one of them alone. A principal bound to no
// seat is answered NotClosed, as every refusal no asking again changes is;
// [mayClose]'s answers are its own.
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

// mayClose is whether actor — a landed change's enqueuer, written for whom
// its enqueue was made for ([enqueuerActor]) — may close item now, decided on
// this node from r's composition of them and chart: nil where it may; a
// [gitlethome.NotClosed], naming who may not and why, where asking again
// changes nothing; any other error where this node cannot say yet, which
// leaves the close owed.
//
// THE ENQUEUER FIRST, then WITHIN WHOM THE CLOSE IS WRITTEN FOR: the
// enqueuer's own authority to update a work item ([authz.Decide], what a
// close made for nobody is decided by alone), and then, where the actor
// states somebody, whether they use the app the verb belongs to, held to the
// ceiling their carrier composes on this node ([builtin.WorkingForCeiling],
// [authz.DecideWithinApps]) — the decision a call of the enqueuing turn would
// get now. A seat the organisation no longer holds, the enqueuer's or the
// requester's, is the composer's definitive answer ([errNoSuchSeat], wrapped),
// told apart from a read that failed.
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

// refusedWithin is the sentence a close refused within the apps of whom,
// for whom enqueuer queued the change, reads beside `merged` to the change's
// authors and its enqueuer: somebody the company does not know, who uses no
// app; somebody who may not act in the company now; or somebody who does not
// use the app the close needs, named, since an administrator giving it to
// them is what changes the answer.
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

// enqueuerActor is who a queued merge's step 2 writes as: a seat as itself,
// written for whom its enqueue was made for (workingFor, nil for nobody: the
// close is the enqueuing turn's write made late, [types.ForTurn] of it, as
// the turn's own writes state), and a person as the human seat they hold with
// the credential they enqueued through beside it, who writes for nobody but
// themselves. A credential bound to no seat enqueues nothing (a merge is a
// bound principal's act), so one here is refused rather than given a name of
// its own.
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
