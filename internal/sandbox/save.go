package sandbox

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// A running job's session, saved as it works: the package doc's "A save taken
// as its job works" has the argument, and "A save under way" and "A save's
// license" what the run's row makes of a save.

// SessionSaver is what a runner offers a save of a job still working: its
// harness's own pack, how far its job's event stream ran, the take every
// capture makes, and the release of a save no take will read.
type SessionSaver interface {
	SessionTaker
	// Saves reports whether its harness packs a save of a running job.
	Saves() bool
	// Streamed is how far the event stream of box's job has run, in bytes.
	Streamed(ctx context.Context, box Sandbox) (int64, error)
	// PackSave packs a save of box's session for taker, at a path of its
	// own; a zero archive where the box holds no session yet.
	PackSave(ctx context.Context, box Sandbox, taker string) (SessionArchive, error)
	// ReleaseSave removes the save at path from box; a path that is no save
	// is refused.
	ReleaseSave(ctx context.Context, box Sandbox, path string) error
}

// The points a save is taken at, as its stamp's lines name them.
const (
	savePoint      = "save"
	drainSavePoint = "drain_save"
)

// saveRetryCeiling caps a failing save's backoff (one attempt an hour at
// worst), and is the longest a paced or roomless seat waits before its room
// is read again.
const saveRetryCeiling = time.Hour

// saveState is this coordinator's part in saving its runs' sessions as they
// work.
type saveState struct {
	// ledger is the fleet's ledger of each seat's saves
	// ([CoordinatorOptions.Saves]); nil counts, and so takes, none.
	ledger SaveLedgers
}

// saveClass is what came of one save task, as the attempt memory reads it:
// never written anywhere, so it carries no wire spelling.
type saveClass string

const (
	// saveNotMoved: the job's stream has not moved since this node's last
	// landed save.
	saveNotMoved saveClass = "not_moved"
	// saveGone: the row is gone, or names another job or box.
	saveGone saveClass = "gone"
	// saveStoodDown: the duty, the drain's end or the coordinator ended the
	// task before its reservation, or the coordinator stopped before its keep.
	saveStoodDown saveClass = "stood_down"
	// saveNotYet: something the save needs is not there now (the box, the
	// license, the policy, the ledger, the taker, the place).
	saveNotYet saveClass = "not_yet"
	// savePaced: the seat's allowance holds it back.
	savePaced saveClass = "paced"
	// saveNothing: the box holds no session to save yet.
	saveNothing saveClass = "nothing"
	// saveFailed: the save was tried and failed.
	saveFailed saveClass = "failed"
	// saveUnresolved: the keep's record was sent and not answered.
	saveUnresolved saveClass = "unresolved"
	// saveLanded: the keep landed.
	saveLanded saveClass = "landed"
	// saveRaced is a charge's lost race ([Coordinator.chargeSave]), read
	// again there: never a task's outcome.
	saveRaced saveClass = "raced"
)

// saveJob is what one save task is handed: the run as its offer found it, the
// stint of duty that offer came under, and what this node's memory says of the
// run's saves.
type saveJob struct {
	run PendingRun
	// stint ends the task's box phase once done: the duty the offer came
	// under. Nil ends it with the coordinator alone.
	stint context.Context
	// drainEnd is a drain save's end, bounding its box phase and its take;
	// zero for a periodic save, the only kind paced.
	drainEnd time.Time
	// due is when the save fell due on this node's clock, for its lateness.
	due time.Time
	// mark is the job's stream length at this node's last landed save of
	// the launch; -1 for none, which reads as moved.
	mark int64
	// interval is the company's save interval, below which pacing holds
	// nothing back.
	interval time.Duration
	// last is the last attempt's start (or its first-sight estimate), which
	// pacing counts from.
	last time.Time
	// counted is what this launch's last save to reach its charge was
	// counted at, pacing's expected size before any save of it lands.
	counted int64
	// taken, where set, is closed once the task has passed its spool or
	// ended before it.
	taken chan struct{}
}

// saveOutcome is what came of a save task, for its memory.
type saveOutcome struct {
	class saveClass
	why   string
	// streamed is the job's stream length read before the pack; -1 where
	// it could not be read.
	streamed int64
	// counted is what the save was counted at, where it reached its charge.
	counted int64
	// pacedUntil is where a paced save is due again.
	pacedUntil time.Time
}

// saveTaken is what a save task holds once its session is in hand.
type saveTaken struct {
	box     Sandbox
	saver   SessionSaver
	row     PendingRun
	policy  SessionPolicy
	archive SessionArchive
	counted int64
	// sealed is the size of the object the keep landed.
	sealed int64
}

// saveTask takes, stamps, spools and keeps one save of job's run, cheapest
// step first (the package doc's "A save taken as its job works"), and answers
// what came of it once its keep has stamped its outcome. Its spawner has
// counted it in c.inflight; it is counted there exactly once until its keep
// starts, and with the keeps from then on, so a stop that comes first cuts it
// within the stop's grace and one that comes after gives its keep the keeps'
// drain. It writes nothing to memory: its outcome is its spawner's.
func (c *Coordinator) saveTask(job saveJob) saveOutcome {
	inflight := sync.OnceFunc(c.inflight.Done)
	defer inflight()
	passed := sync.OnceFunc(func() {
		if job.taken != nil {
			close(job.taken)
		}
	})
	defer passed()
	start := c.now()
	point := savePoint
	if !job.drainEnd.IsZero() {
		point = drainSavePoint
	}
	duty, endDuty := context.WithCancel(c.life)
	defer endDuty()
	if job.stint != nil {
		defer context.AfterFunc(job.stint, endDuty)()
	}
	if !job.drainEnd.IsZero() {
		var endDrain context.CancelFunc
		duty, endDrain = context.WithDeadline(duty, job.drainEnd)
		defer endDrain()
	}
	out := saveOutcome{streamed: -1}
	in, out := c.saveInHand(duty, job, out)
	if in != nil {
		out = c.takeSave(job, point, in, out, passed, inflight)
	}
	switch out.class {
	case saveLanded:
		log.InfoContext(c.life, "executor_session_saved", "turn_id", job.run.TurnID, "agent", job.run.AgentHandle,
			"launch_seq", in.row.LaunchSeq, "capture_seq", nextCaptureSeq(in.row), "point", point,
			"late_s", start.Sub(job.due).Seconds(), "took_s", c.now().Sub(start).Seconds(),
			"counted", out.counted, "bytes", in.sealed)
	case saveNotMoved, savePaced, saveNotYet:
		log.DebugContext(c.life, "executor_session_save_deferred", "turn_id", job.run.TurnID, "point", point,
			"class", string(out.class), "why", out.why)
	}
	return out
}

// saveInHand is a save's steps up to its charge: the box attached, its stream,
// the one read of the row, the policy, the seat's room, what the box was
// handed, the taker, the pack, and the charge. Nil, with the outcome, where
// the task ends there; the pack is released from its box on every such end.
func (c *Coordinator) saveInHand(duty context.Context, job saveJob, out saveOutcome) (*saveTaken, saveOutcome) {
	run := job.run
	packed := false
	// STOOD DOWN where the coordinator stopped, or the duty ended while the
	// box phase ran: past the pack the save is the coordinator's.
	end := func(class saveClass, why string) (*saveTaken, saveOutcome) {
		if (class == saveNotYet || class == saveFailed) && (c.life.Err() != nil || (!packed && duty.Err() != nil)) {
			class = saveStoodDown
		}
		out.class, out.why = class, why
		return nil, out
	}
	if c.saves.ledger == nil || !c.keepsSessions() {
		return end(saveNotYet, "this node keeps no executor log or no seat's save ledger")
	}
	c.sweepSpools()
	manager := c.mgr()
	if manager == nil {
		return end(saveNotYet, "this node has no sandbox manager")
	}
	box, runner, err := manager.Attach(duty, Placement(run.Placement), run.SandboxID, run.Harness)
	if err != nil {
		return end(saveNotYet, fmt.Sprintf("the run's box could not be attached: %v", err))
	}
	saver, ok := runner.(SessionSaver)
	if !ok || !saver.Saves() {
		return end(saveNotYet, "the run's harness takes no save of a running job")
	}
	// RULE 6: an unreadable stream reads as moved, and the pack is the test.
	if streamed, streamErr := saver.Streamed(duty, box); streamErr == nil {
		out.streamed = streamed
		if streamed <= job.mark {
			return end(saveNotMoved, "the job's stream has not moved since its last save here")
		}
	}
	if duty.Err() != nil {
		return end(saveStoodDown, "the duty ended before the save read its row")
	}
	row, class, why := c.saveRow(job)
	if class != "" {
		return end(class, why)
	}
	policy, why := c.savePolicy()
	if why != "" {
		return end(saveNotYet, why)
	}
	room, why := c.seatSaveRoom(c.life, row.AgentHandle, policy.SeatBudget)
	if why != "" {
		return end(saveNotYet, why)
	}
	if job.drainEnd.IsZero() {
		if until, paced := paceSave(row, job, room, c.now()); paced {
			out.pacedUntil = until
			return end(savePaced, fmt.Sprintf("the seat's saves leave %d bytes of its budget", room))
		}
	}
	if unopened := c.saveHanded(row); unopened != "" {
		return end(saveFailed, unopened)
	}
	hold, held := c.heldTaker()
	if !held {
		return end(saveNotYet, takerUnheld)
	}
	archive, err := saver.PackSave(duty, box, hold.holder)
	switch {
	case err != nil:
		return end(saveFailed, fmt.Sprintf("the box's session could not be packed: %v", err))
	case archive.Path == "":
		return end(saveNothing, "the box holds no session yet")
	}
	packed = true
	release := func() {
		rel, cancel := detached(c.life)
		defer cancel()
		_ = saver.ReleaseSave(rel, box, archive.Path)
	}
	counted := countedFor(archive.Size)
	entry := savedEntry{ID: uuid.New(), Until: c.now().UTC().Add(saveLedgerHold), Bytes: counted}
	if refused, refusal := c.chargeSave(c.life, row.AgentHandle, entry, policy.SeatBudget); refused != "" {
		release()
		if refused == savePaced {
			out.pacedUntil = c.now().Add(saveRetryCeiling)
		}
		return end(refused, refusal)
	}
	out.counted = counted
	return &saveTaken{box: box, saver: saver, row: row, policy: policy, archive: archive, counted: counted}, out
}

// saveRow is rule 7: the ONE READ of the run's row a save's place, save point
// and cursor come from, made before its pack; settled where a save of the
// turn was left begun by a gone taker or unanswered, and held to the
// license's pre-check, so a pack the store would refuse is spared. A class
// where the save ends here.
func (c *Coordinator) saveRow(job saveJob) (PendingRun, saveClass, string) {
	run := job.run
	read, cancel := c.rowContext(c.life, c.recordBudget)
	row, found, err := c.pending.Get(read, run.TurnID)
	cancel()
	ours := func(r PendingRun) bool {
		return r.LaunchID == run.LaunchID && r.SandboxID == run.SandboxID && r.LaunchSeq > 0
	}
	switch {
	case err != nil:
		return row, saveNotYet, fmt.Sprintf("the run's record could not be read: %v", err)
	case !found || !ours(row):
		return row, saveGone, "the run's row is gone, or names another job or box"
	}
	settle, cancel := c.rowContext(c.life, c.recordBudget)
	defer cancel()
	if row, found, _ = c.settleGone(settle, row, askEvery, false); !found || !ours(row) {
		return row, saveGone, "the run's row is gone, or names another job or box"
	}
	row = c.settleUnresolved(settle, row, askEvery)
	if !ours(row) {
		return row, saveGone, "the run's row names another job or box"
	}
	if why := licensesSave(row, SessionCapture{Launch: row.LaunchID, LaunchSeq: row.LaunchSeq,
		CaptureSeq: nextCaptureSeq(row)}, c.now()); why != "" {
		return row, saveNotYet, why
	}
	return row, "", ""
}

// savePolicy is the fleet's policy a save is taken under, or why none is now:
// unreadable, or keeping no session.
func (c *Coordinator) savePolicy() (SessionPolicy, string) {
	read, cancel := context.WithTimeout(c.life, saveLedgerWait)
	defer cancel()
	policy, err := c.keep.Policy(read)
	switch {
	case err != nil:
		return policy, fmt.Sprintf("the company's session policy could not be read off the revision the "+
			"fleet runs: %v", err)
	case !policy.Keep:
		return policy, "the company keeps no session"
	}
	return policy, ""
}

// saveHanded is rule 9: why what row's box was handed cannot be read and
// opened, "" where it can. Checked before the pack, so a set that is missing or
// marked incomplete (which stays so for the launch) costs the box nothing;
// the scrub's own values are read after the pack.
func (c *Coordinator) saveHanded(row PendingRun) string {
	read, cancel := c.rowContext(c.life, c.recordBudget)
	defer cancel()
	sealed, err := c.pending.Handed(read, row)
	switch {
	case err != nil:
		return fmt.Sprintf("what the run's box was handed could not be read: %v", err)
	case sealed == "":
		return "the run's record keeps nothing its box was handed, so no save of it can be scrubbed"
	}
	if _, openErr := c.keep.OpenHanded(row.TurnID, sealed); openErr != nil {
		return fmt.Sprintf("what the run's box was handed could not be opened: %v", openErr)
	}
	return ""
}

// paceSave is rule 8's pacing of a periodic save: held back while the seat's
// room, spent at the session's expected size once per hold, would not last
// the interval, until min(last + paced, now + [saveRetryCeiling]). The
// expected size is this launch's newest landed save's, else what its last
// save was counted at; another launch's is never read.
func paceSave(row PendingRun, job saveJob, room int64, now time.Time) (time.Time, bool) {
	ceiling := now.Add(saveRetryCeiling)
	if room <= 0 {
		return ceiling, true
	}
	expected := job.counted
	if kept := launchKept(row); kept != nil {
		expected = kept.ObjectSize
	}
	if expected <= 0 {
		return time.Time{}, false
	}
	// IN FLOATS: a hold of nanoseconds times a session's bytes passes int64.
	paced := float64(saveLedgerHold) * float64(expected) / float64(room)
	if paced <= float64(job.interval) {
		return time.Time{}, false
	}
	remaining := paced - float64(now.Sub(job.last))
	switch {
	case remaining <= 0:
		return time.Time{}, false
	case remaining >= float64(saveRetryCeiling):
		return ceiling, true
	}
	return now.Add(time.Duration(remaining)), true
}

// takeSave is a save's steps past its charge: the take, its stamp at the
// launch's newest save point, its reservation, its spool and its keep. passed
// marks the spool passed; inflight hands the task from c.inflight to the
// keeps.
func (c *Coordinator) takeSave(job saveJob, point string, in *saveTaken, out saveOutcome,
	passed, inflight func()) saveOutcome {

	run, row := job.run, in.row
	at := capturePoint{name: point, pack: true, cutOff: true}
	stopping := func() bool {
		return c.life.Err() != nil || (!job.drainEnd.IsZero() && !time.Now().Before(job.drainEnd))
	}
	end := func(class saveClass, why string) saveOutcome {
		if class == saveFailed && stopping() {
			class = saveStoodDown
		}
		out.class, out.why = class, why
		return out
	}
	release := func() {
		rel, cancel := detached(c.life)
		defer cancel()
		_ = in.saver.ReleaseSave(rel, in.box, in.archive.Path)
	}
	// THE TAKE CONTEXT AFTER THE PACK, so the scrub has its whole budget; a
	// drain's end bounds it after takeContext, never its parent.
	take, cancel, by := c.takeContext(c.life)
	defer cancel()
	if !job.drainEnd.IsZero() {
		var endDrain context.CancelFunc
		take, endDrain = context.WithDeadline(take, job.drainEnd)
		defer endDrain()
	}
	scrub, err := c.scrubValues(take, row, in.box)
	if err != nil {
		release()
		return end(saveFailed, err.Error())
	}
	taken, err := in.saver.TakeSession(take, in.box, SessionTake{Save: in.archive.Path, Scrub: scrub,
		MaxBytes: executorkeep.SessionMaxBytes})
	switch {
	case err != nil:
		return end(saveFailed, takeRefusal(take, err))
	case taken.Body == nil:
		return end(saveFailed, "the save was gone from its box when it was taken")
	}
	defer func() { _ = taken.Body.Close() }()
	hold, held := c.heldTaker()
	if !held {
		return end(saveNotYet, takerUnheld)
	}
	stamp := captureStamp(row, run.LaunchID, nextCaptureSeq(row), false, true, hold, taken, in.box, in.policy)
	stamp.placeAtNewest(savePlaces{run: row})
	stamp.At = c.now().UTC()
	reserveBy, _ := take.Deadline()
	took, _, err := c.askStamp(take, reserveBy, run.TurnID, stamp, c.pending.ReserveSave)
	switch {
	case err != nil:
		// A SAVE NEVER STANDS BY: a place another read holds is that read's.
		if _, beside := c.abandonPlace(take, by, row, stamp, at, err); beside {
			return end(saveNotYet, "another read of the run holds the save's place")
		}
		return end(saveFailed, fmt.Sprintf("the save's place was not reserved within its time: %v", err))
	case !took:
		return end(saveNotYet, "the run's row refused the save's place")
	}
	spooled, err := c.spool(take, row, stamp, taken)
	if err != nil {
		c.finishCapture(take, by, row, stamp, CaptureRefused, takeRefusal(take, err))
		return end(saveFailed, takeRefusal(take, err))
	}
	if size := spooled.spool.Size; size > in.counted {
		_ = os.Remove(spooled.file)
		why := fmt.Sprintf("the session sealed is %d bytes, past the %d its seat's save ledger counted for it",
			size, in.counted)
		c.finishCapture(take, by, row, stamp, CaptureRefused, why)
		return end(saveFailed, why)
	}
	c.spooledWhole(take, by, row, spooled, taken, 0)
	passed()
	if !c.joinKeeps() {
		c.dropSpool(take, spooled, "the node stopped before the session could be uploaded")
		return end(saveStoodDown, "the node stopped before the save could be kept")
	}
	inflight()
	defer c.keeping.Done()
	kept := c.keepCapture(c.keepLife, spooled)
	switch {
	case kept == nil:
		return end(saveFailed, "the save's keep kept nothing")
	case kept.NotKept != "":
		return end(saveFailed, kept.NotKept)
	case kept.Bytes > 0:
		in.sealed = kept.Bytes
		return end(saveLanded, "")
	}
	return end(saveUnresolved, "the save's record was sent and not answered")
}
