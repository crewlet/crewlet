package sandbox

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
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
	// own; a zero archive where the box holds no session yet, an error
	// wrapping [ErrSaveNoRoom] where the box lacks the memory to pack it, and
	// an [UnrestorableError], nothing packed, where no box continues it.
	// session is a minting harness's bound session ([SessionTake.Session]),
	// and a save whose box names another is refused, as a take is.
	PackSave(ctx context.Context, box Sandbox, taker, session string) (SessionArchive, error)
	// ReleaseSave removes the save at path from box; a path that is no save
	// is refused.
	ReleaseSave(ctx context.Context, box Sandbox, path string) error
}

// ErrSaveNoRoom is a pack a box refused because its memory domain lacks the
// room the pack is estimated to take: the save waits, never doubled, and is
// due again at one interval, since the room is the box's now, not the save's.
var ErrSaveNoRoom = errors.New("the box lacks the memory to pack its session now")

// The points a save is taken at, as its stamp's lines name them.
const (
	savePoint      = "save"
	drainSavePoint = "drain_save"
)

// saveRetryCeiling caps a failing save's backoff (one attempt an hour at
// worst), and is the longest a paced or roomless seat waits before its room
// is read again.
const saveRetryCeiling = time.Hour

// maxConcurrentSaves is how many saves a node takes at once, each from its
// offer to its keep's end, for its waiter's offers and its drain's saves
// together: it bounds the packs and the scrubs (a pack of 1 to 20 MiB takes
// 0.04 to 0.46 s of a core), and sizes the room every spool shares
// ([spoolRoomBytes]). A run's final captures take none. A judgment for a
// container's exec hop, which the saved line's took_s measures.
const maxConcurrentSaves = 4

// drainSaveWait is the longest a draining node saves its Direct jobs'
// sessions for: a take of an ordinary session is seconds, and the wait runs
// beside the drain's wait for turns, within a seat pod's grace. A judgment,
// which a drain save's late_s and took_s measure.
const drainSaveWait = 30 * time.Second

// saveState is this coordinator's part in saving its runs' sessions as they
// work.
type saveState struct {
	// ledger is the fleet's ledger of each seat's saves
	// ([CoordinatorOptions.Saves]); nil counts, and so takes, none.
	ledger SaveLedgers
	// slots is [maxConcurrentSaves], taken without waiting by an offer, and
	// waiting by a drain, and given back once its task's keep has ended.
	slots chan struct{}

	// mu guards memos, swept and draining: this node's memory of each turn's
	// saves, by turn, when it last forgot the ones no offer came for, and
	// whether the node drains ([Coordinator.SaveHosted]), which admits no
	// offer. Never c.mu, so an offer from the poll waits on no run's work.
	mu       sync.Mutex
	memos    map[string]*saveMemo
	swept    time.Time
	draining bool
}

// saveMemo is this node's memory of one turn's saves, every instant on this
// node's clock (the package doc's "A save is offered by the poll, from
// memory"). Kept nowhere else, so a node that restarts starts it at first
// sight.
type saveMemo struct {
	// launch and box are the job and box it counts; an offer of another
	// starts it again.
	launch, box string
	// offered is the last offer's instant, which forgetting reads.
	offered time.Time
	// last is the last attempt's start, or first sight's estimate of it.
	last time.Time
	// failures counts the attempts in a row that failed.
	failures int
	// pacedUntil is where the seat's allowance holds the next save back to;
	// zero unpaced.
	pacedUntil time.Time
	// mark is the job's stream length read at the start of this node's last
	// landed save of the job; -1 for none.
	mark int64
	// counted is what the job's last save to reach its charge, or to be
	// paced after its pack, was counted at.
	counted int64
	// running is the task under way on this node, nil for none.
	running *saveRun
	// logged is every kind of failure already logged at Warn for the job.
	logged map[string]bool
	// unrestorable says a save of the job found its session one no box of this
	// build continues ([saveUnrestorable]): no save of it is offered again.
	unrestorable bool
}

// of reports whether m is the memory of run's job and box.
func (m *saveMemo) of(run PendingRun) bool {
	return m != nil && m.launch == run.LaunchID && m.box == run.SandboxID
}

// saveRun is a save task under way on this node: taken is closed once it has
// passed its spool and joined the keeps, or ended before, ended once its
// outcome is in memory and its slot given back.
type saveRun struct {
	taken chan struct{}
	ended chan struct{}
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
	// saveStoodDown: its offerer's stop, the drain's end or the coordinator
	// ended the task before its reservation, or the coordinator stopped
	// before its keep.
	saveStoodDown saveClass = "stood_down"
	// saveNotYet: something the save needs is not there now (the box, the
	// license, the policy, the ledger, the taker, the box's room for its
	// pack, the place).
	saveNotYet saveClass = "not_yet"
	// savePaced: the seat's allowance holds it back.
	savePaced saveClass = "paced"
	// saveNothing: the box holds no session to save yet.
	saveNothing saveClass = "nothing"
	// saveFailed: the save was tried and failed.
	saveFailed saveClass = "failed"
	// saveUnrestorable: no box of this build continues the job's session
	// ([UnrestorableError]), and none will while the job runs: the release that
	// wrote it is the record's, made before its harness started.
	saveUnrestorable saveClass = "unrestorable"
	// saveUnresolved: the keep's record was sent and not answered.
	saveUnresolved saveClass = "unresolved"
	// saveLanded: the keep landed.
	saveLanded saveClass = "landed"
	// saveRaced is a charge's lost race ([Coordinator.chargeSave]), read
	// again there: never a task's outcome.
	saveRaced saveClass = "raced"
)

// saveJob is what one save task is handed beside its offerer: the run as its
// offer found it, and what this node's memory says of the run's saves.
type saveJob struct {
	run PendingRun
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
	// counted is what this launch's last save to reach its charge, or to be
	// paced after its pack, was counted at: what pacing expects ([expectedSave]).
	counted int64
	// taken, where set, is closed once the task has passed its spool and
	// joined the keeps, or ended before.
	taken chan struct{}
}

// point is the point job's save is taken at, as its stamp's lines name it.
func (j saveJob) point() string {
	if j.drainEnd.IsZero() {
		return savePoint
	}
	return drainSavePoint
}

// saveOutcome is what came of a save task, for its memory.
type saveOutcome struct {
	class saveClass
	why   string
	// streamed is the job's stream length read before the pack; -1 where
	// it could not be read.
	streamed int64
	// counted is what the save was counted at, where it reached its charge
	// or was paced after its pack.
	counted int64
	// pacedUntil is where a paced save is due again.
	pacedUntil time.Time
	// unplaced names the step a failure came at before the save's place was
	// asked ("handed", "pack", "scrub", "take"), "ledger" for a seat whose
	// save ledger a newer build owns, which takes no save until it is gone,
	// "room" for a box that lacked the room to pack ([ErrSaveNoRoom]), or
	// "spool" for a node whose spools filled their room ([spoolRoomBytes]):
	// what no stamp's line logs. "" for every other outcome.
	unplaced string
	// loss is the unplaced failure in the one vocabulary where it is not
	// [executorkeep.LossKeepFailed]: a session past its bound or one no box
	// continues, a spool with no room.
	loss executorkeep.Loss
}

// saveTaken is what a save task holds once its session is in hand.
type saveTaken struct {
	box     Sandbox
	saver   SessionSaver
	row     PendingRun
	policy  SessionPolicy
	archive SessionArchive
	counted int64
	// charge is its entry on its seat's save ledger, which its keep amends
	// to what it uploads.
	charge *saveCharge
	// room is the room its spools hold, its scrub's and its sealed ones',
	// from before its pack ([takeRoomBytes]).
	room *spoolHold
	// sealed is the size of the object the keep landed.
	sealed int64
}

// saveTask takes, stamps, spools and keeps one save of job's run, cheapest
// step first (the package doc's "A save taken as its job works"), and answers
// what came of it once its keep has stamped its outcome. Its spawner has
// counted it in c.inflight; it is counted there exactly once until its keep
// starts, and with the keeps from then on, so a stop that comes first cuts it
// within the stop's grace and one that comes after gives its keep the keeps'
// drain. It runs on ctx, and phase bounds its box phase
// ([Coordinator.saveContexts]). It writes nothing to memory: its outcome is
// its spawner's.
func (c *Coordinator) saveTask(ctx, phase context.Context, job saveJob) saveOutcome {
	inflight := sync.OnceFunc(c.inflight.Done)
	defer inflight()
	passed := sync.OnceFunc(func() {
		if job.taken != nil {
			close(job.taken)
		}
	})
	defer passed()
	start := c.now()
	point := job.point()
	out := saveOutcome{streamed: -1}
	in, out := c.saveInHand(ctx, phase, job, out)
	if in != nil {
		// ITS ROOM BACK ONCE ITS KEEP HAS ENDED, as its spool's removal gives
		// it, or where none was spooled.
		defer in.room.release()
		out = c.takeSave(ctx, job, point, in, out, passed, inflight)
	}
	switch out.class {
	case saveLanded:
		log.InfoContext(ctx, "executor_session_saved", "turn_id", job.run.TurnID, "agent", job.run.AgentHandle,
			"launch_seq", in.row.LaunchSeq, "capture_seq", nextCaptureSeq(in.row), "point", point,
			"late_s", start.Sub(job.due).Seconds(), "took_s", c.now().Sub(start).Seconds(),
			"counted", out.counted, "amended", in.charge.entry.Bytes, "bytes", in.sealed)
	case saveNotMoved, savePaced, saveNotYet:
		log.DebugContext(ctx, "executor_session_save_deferred", "turn_id", job.run.TurnID, "point", point,
			"class", string(out.class), "why", out.why)
	}
	return out
}

// saveContexts is what a save task runs on: life, the coordinator's own
// lifetime, which its take and keep go on under past its offerer; and phase,
// which bounds its box phase and ends with offerer (the life of the waiter that
// offered it), a drain's end (drainEnd, zero for none) or the coordinator.
// cancel ends phase.
func (c *Coordinator) saveContexts(offerer context.Context, drainEnd time.Time) (life, phase context.Context,
	cancel context.CancelFunc) {

	phase, endPhase := context.WithCancel(c.life)
	stop := context.AfterFunc(offerer, endPhase)
	cancel = func() {
		stop()
		endPhase()
	}
	if !drainEnd.IsZero() {
		var endDrain context.CancelFunc
		phase, endDrain = context.WithDeadline(phase, drainEnd)
		endOffered := cancel
		cancel = func() {
			endDrain()
			endOffered()
		}
	}
	return c.life, phase, cancel
}

// saveInHand is a save's steps up to its charge: the box attached, its stream,
// the one read of the row, the policy, the seat's room, what the box was
// handed, the taker, the pack, and the charge, on ctx, the box's steps under
// phase. Nil, with the outcome, where the task ends there; the pack is
// released from its box on every such end.
func (c *Coordinator) saveInHand(ctx, phase context.Context, job saveJob, out saveOutcome) (*saveTaken,
	saveOutcome) {

	run := job.run
	packed := false
	var spooling *spoolHold
	// STOOD DOWN where the coordinator stopped, or the phase ended while the
	// box phase ran: past the pack the save is the coordinator's.
	end := func(class saveClass, why string) (*saveTaken, saveOutcome) {
		spooling.release()
		if (class == saveNotYet || class == saveFailed) && (ctx.Err() != nil || (!packed && phase.Err() != nil)) {
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
	box, runner, err := manager.Attach(phase, Placement(run.Placement), run.SandboxID, run.Harness)
	if err != nil {
		return end(saveNotYet, fmt.Sprintf("the run's box could not be attached: %v", err))
	}
	saver, ok := runner.(SessionSaver)
	if !ok || !saver.Saves() {
		return end(saveNotYet, "the run's harness takes no save of a running job")
	}
	// RULE 6: an unreadable stream reads as moved, and the pack is the test.
	if streamed, streamErr := saver.Streamed(phase, box); streamErr == nil {
		out.streamed = streamed
		if streamed <= job.mark {
			return end(saveNotMoved, "the job's stream has not moved since its last save here")
		}
	}
	if phase.Err() != nil {
		return end(saveStoodDown, "the box phase ended before the save read its row")
	}
	row, class, why := c.saveRow(ctx, job)
	if class != "" {
		return end(class, why)
	}
	// A MINTING JOB'S SESSION IS BOUND BEFORE IT IS PACKED (minted.go).
	if row, err = c.bindMinted(ctx, row, box, runner); errors.Is(err, errNoSessionReported) {
		return end(saveNothing, err.Error())
	} else if err != nil {
		return end(saveFailed, fmt.Sprintf("the job's session was not bound: %v", err))
	}
	policy, why := c.savePolicy(ctx)
	if why != "" {
		return end(saveNotYet, why)
	}
	room, err := c.seatSaveRoom(ctx, row.AgentHandle, policy.SeatBudget)
	if err != nil {
		if errors.Is(err, errNewerLedger) {
			out.unplaced = "ledger"
		}
		return end(saveNotYet, err.Error())
	}
	expected := expectedSave(row, job)
	if job.drainEnd.IsZero() {
		if until, paced := paceAt(expected, job, room, c.now()); paced {
			out.pacedUntil = until
			return end(savePaced, fmt.Sprintf("the seat's saves leave %d bytes of its budget", room))
		}
	}
	if unopened := c.saveHanded(ctx, row); unopened != "" {
		out.unplaced = "handed"
		return end(saveFailed, unopened)
	}
	hold, held := c.heldTaker()
	if !held {
		return end(saveNotYet, takerUnheld)
	}
	// ROOM FOR ITS SPOOLS AT THE SEALED BOUND, taken before the pack slot and
	// never waited for: a save that finds none is due again, and leaves a
	// final take the room saves never fill ([spoolRoomBytes]).
	if spooling = c.trySpoolRoom(takeRoomBytes); spooling == nil {
		out.unplaced, out.loss = "spool", executorkeep.LossNoSpoolRoom
		return end(saveNotYet, fmt.Sprintf("this node's session spools fill their room (%d bytes)",
			spoolRoomBytes))
	}
	// ONE PACK AT A TIME in a box that shares the node's memory
	// ([Coordinator.sharedPack]), given back as the pack returns.
	give, err := c.packSlot(phase, row)
	if err != nil {
		return end(saveStoodDown, err.Error())
	}
	archive, err := saver.PackSave(phase, box, hold.holder, row.Launch.Minted)
	give()
	switch {
	case errors.As(err, new(*UnrestorableError)):
		loss, why := takeRefusal(phase, err)
		out.unplaced, out.loss = "pack", loss
		return end(saveUnrestorable, why)
	case errors.Is(err, ErrSaveNoRoom):
		out.unplaced = "room"
		return end(saveNotYet, fmt.Sprintf("the box's session was not packed: %v", err))
	case err != nil:
		out.unplaced = "pack"
		return end(saveFailed, fmt.Sprintf("the box's session could not be packed: %v", err))
	case archive.Path == "":
		return end(saveNothing, "the box holds no session yet")
	}
	packed = true
	release := func() {
		rel, cancel := detached(ctx)
		defer cancel()
		_ = saver.ReleaseSave(rel, box, archive.Path)
	}
	counted := countedFor(archive.Size, boundOf(policy.kept()).sealed)
	// PACED AT WHAT THE PACK MEASURED, before its charge, where nothing told
	// the session's size before it ([expectedSave]): released uncharged, its
	// size kept so the next is paced before its pack. Never again past an
	// estimate, which a growing session would outrun pack after pack; a drain
	// saves what it has.
	if job.drainEnd.IsZero() && expected <= 0 {
		if until, paced := paceAt(counted, job, room, c.now()); paced {
			release()
			out.counted, out.pacedUntil = counted, until
			return end(savePaced, fmt.Sprintf("the seat's saves leave %d bytes of its budget, and the "+
				"session packed at %d", room, counted))
		}
	}
	spooling.counting(counted)
	// CHARGED WHAT IT WILL MOST LIKELY UPLOAD: its archive, less the media
	// its turn's newest kept capture holds, which its keep carries rather
	// than uploads; amended once the keep knows ([Coordinator.amendSave]).
	charge := &saveCharge{seat: row.AgentHandle, budget: policy.SeatBudget,
		entry: savedEntry{ID: uuid.New(), Until: c.now().UTC().Add(saveLedgerHold), Bytes: counted}}
	if row.Kept != nil {
		charge.entry.Bytes = max(counted-row.Kept.mediaBytes(), saveMarginFloor)
	}
	if refused, refusal := c.chargeSave(ctx, charge.seat, charge.entry, charge.budget); refused != "" {
		release()
		if refused == savePaced {
			out.pacedUntil = c.now().Add(saveRetryCeiling)
		}
		return end(refused, refusal)
	}
	out.counted = charge.entry.Bytes
	return &saveTaken{box: box, saver: saver, row: row, policy: policy, archive: archive, counted: counted,
		charge: charge, room: spooling}, out
}

// saveRow is rule 7: the ONE READ of the run's row a save's place, save point
// and cursor come from, made before its pack; settled where a save of the
// turn was left begun by a gone taker or unanswered, and held to the
// license's pre-check, so a pack the store would refuse is spared. A class
// where the save ends here.
func (c *Coordinator) saveRow(ctx context.Context, job saveJob) (PendingRun, saveClass, string) {
	run := job.run
	read, cancel := c.rowReadContext(ctx, c.recordBudget)
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
	settle, cancel := c.rowContext(ctx, c.recordBudget)
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
func (c *Coordinator) savePolicy(ctx context.Context) (SessionPolicy, string) {
	read, cancel := context.WithTimeout(ctx, saveLedgerWait)
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
func (c *Coordinator) saveHanded(ctx context.Context, row PendingRun) string {
	read, cancel := c.rowReadContext(ctx, c.recordBudget)
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

// expectedSave is the size rule 8 paces a launch's next save at before its
// pack: this launch's newest landed save's, else what this node last counted
// of it, else the kept capture the launch continued ([continuedKept]), the
// one other launch's capture it reads. 0 where none says, and the pack's own
// size paces it.
func expectedSave(row PendingRun, job saveJob) int64 {
	expected := job.counted
	if kept := launchKept(row); kept != nil {
		expected = kept.ObjectSize
	}
	if kept := continuedKept(row); expected <= 0 && kept != nil {
		expected = kept.ObjectSize
	}
	return expected
}

// continuedKept is run's kept capture where its launch continued it
// ([LaunchRecord.Continued]): that launch and save point. Nil otherwise: the
// box's own copy and the conversation's base are sized by no row, so their
// first save is paced after its pack.
func continuedKept(run PendingRun) *SessionCapture {
	p, kept := run.Launch.Continued, run.Kept
	if p == nil || p.LaunchSeq <= 0 || kept == nil || kept.LaunchSeq != p.LaunchSeq {
		return nil
	}
	if at, stated := kept.SavePoint(); !stated || at != p.Through {
		return nil
	}
	return kept
}

// paceAt is rule 8's pacing of a periodic save of a session of expected
// bytes: held back while the seat's room, spent at that size once per hold,
// would not last the interval, until min(last + paced, now +
// [saveRetryCeiling]).
func paceAt(expected int64, job saveJob, room int64, now time.Time) (time.Time, bool) {
	ceiling := now.Add(saveRetryCeiling)
	if room <= 0 {
		return ceiling, true
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

// takeSave is a save's steps past its charge, on ctx: the take, its stamp at
// zero, its reservation, its spool and its raise, and its keep.
// passed marks the spool passed and the keep counted; inflight hands the task
// from c.inflight to the keeps.
func (c *Coordinator) takeSave(ctx context.Context, job saveJob, point string, in *saveTaken, out saveOutcome,
	passed, inflight func()) saveOutcome {

	run, row := job.run, in.row
	at := capturePoint{name: point, pack: true, cutOff: true}
	stopping := func() bool {
		return ctx.Err() != nil || (!job.drainEnd.IsZero() && !time.Now().Before(job.drainEnd))
	}
	end := func(class saveClass, why string) saveOutcome {
		if class == saveFailed && stopping() {
			class = saveStoodDown
		}
		out.class, out.why = class, why
		return out
	}
	release := func() {
		rel, cancel := detached(ctx)
		defer cancel()
		_ = in.saver.ReleaseSave(rel, in.box, in.archive.Path)
	}
	// THE TAKE CONTEXT AFTER THE PACK, so the scrub has its whole budget; a
	// drain's end bounds it after takeContext, never its parent.
	take, cancel, by := c.takeContext(ctx, boundOf(in.policy.kept()).take)
	defer cancel()
	if !job.drainEnd.IsZero() {
		var endDrain context.CancelFunc
		take, endDrain = context.WithDeadline(take, job.drainEnd)
		defer endDrain()
	}
	scrub, err := c.scrubValues(take, row, in.box)
	if err != nil {
		release()
		out.unplaced = "scrub"
		return end(saveFailed, err.Error())
	}
	dir, err := c.spoolHere()
	if err != nil {
		release()
		out.unplaced = "spool"
		return end(saveFailed, err.Error())
	}
	taken, err := in.saver.TakeSession(take, in.box, in.policy.Bound(SessionTake{Save: in.archive.Path,
		Scrub: scrub, Scope: row.AgentID, Session: row.Launch.Minted, Spool: in.room.scrubbing(dir)}))
	switch {
	case err != nil:
		out.unplaced = "take"
		loss, why := takeRefusal(take, err)
		out.loss = loss
		if errors.As(err, new(*UnrestorableError)) {
			return end(saveUnrestorable, why)
		}
		return end(saveFailed, why)
	case taken.Body == nil:
		out.unplaced = "take"
		return end(saveFailed, "the save was gone from its box when it was taken")
	}
	defer func() { _ = taken.Body.Close() }()
	hold, held := c.heldTaker()
	if !held {
		return end(saveNotYet, takerUnheld)
	}
	stamp := captureStamp(row, run.LaunchID, nextCaptureSeq(row), false, true, hold, taken, in.box, in.policy)
	// RESERVED AT ZERO, its instant last, and raised once its spool is
	// counted ([SessionCapture.placeAtNewest] says why), off this one row read.
	stamp.placeAtZero(row.Launch)
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
	// NEVER PAST ITS ROOM, shrunk to its archive counted ([countedFor]): the
	// seal is refused before a byte past it lands, and the ledger is held to
	// what the keep uploads ([Coordinator.amendSave]).
	spooled, err := c.spool(take, row, stamp, taken, &savePlaces{run: row, resumeRead: taken.ResumeRead,
		needsResumeRead: placesWithResumeRead(in.saver)}, in.room)
	if err != nil {
		loss, why := takeRefusal(take, err)
		if errors.Is(err, errSpoolPastHold) {
			why = fmt.Sprintf("the session sealed is past the %d bytes its spools' room held for it",
				in.counted)
		}
		c.finishCapture(take, by, row, stamp, CaptureRefused, loss, why)
		return end(saveFailed, why)
	}
	spooled.charge = in.charge
	c.spooledWhole(take, by, row, spooled, taken, 0)
	if !c.joinKeeps() {
		// A SAVE NEVER STANDS BY, so its spool is refused, never withdrawn.
		c.finishCapture(take, time.Time{}, spooled.run, spooled.stamp, CaptureRefused, executorkeep.LossKeepFailed,
			"the node stopped before the session could be uploaded")
		spooled.remove()
		return end(saveStoodDown, "the node stopped before the save could be kept")
	}
	inflight()
	// PASSED ONCE COUNTED WITH THE KEEPS, so a drain let go here leaves no
	// stop a moment to refuse the keep it promised goes on.
	passed()
	kept := c.keepSave(spooled)
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

// keepSave keeps a save's spool on the keeps' own lifetime, as
// [Coordinator.keepLater] does, never the task's, and counts its keep done;
// its caller has joined it to the keeps.
func (c *Coordinator) keepSave(spooled *spooledCapture) *SessionKept {
	defer c.keeping.Done()
	return c.keepCapture(c.keepLife, spooled)
}

// SaveRunning is offered run's box, which a waiter's poll found running while
// offerer, that waiter's life, lasts, for a save of its job's session as it
// works ([WaiterOptions.Save]).
// It decides from memory alone and returns at once: a save that is due and
// finds a slot free runs as a task of the coordinator's own lifetime (the
// package doc's "A save is offered by the poll, from memory").
func (c *Coordinator) SaveRunning(offerer context.Context, run PendingRun, runner Runner) {
	c.offerSave(offerer, run, runner)
}

// offerSave is [Coordinator.SaveRunning], answering the task it started, nil
// for none.
func (c *Coordinator) offerSave(offerer context.Context, run PendingRun, runner Runner) *saveRun {
	// RULE 1, saves are on: a save names its taker and is charged to a ledger.
	if !c.capturing() || c.taker == nil || c.saves.ledger == nil {
		return nil
	}
	// RULE 2, the runner saves.
	if saver, ok := runner.(SessionSaver); !ok || !saver.Saves() {
		return nil
	}
	// RULE 3, the offering waiter lives, the node is not draining and no
	// task of the turn runs here.
	if offerer == nil || offerer.Err() != nil {
		return nil
	}
	now, interval := c.now(), c.keep.SaveInterval()
	c.saves.mu.Lock()
	if memo := c.saves.memos[run.TurnID]; c.saves.draining || (memo != nil && memo.running != nil) {
		c.saves.mu.Unlock()
		return nil
	}
	memo := c.saves.memoOf(run, now, interval)
	// RULE 4, due, of a session some box continues; RULE 5, a slot free now, or
	// the next poll offers again.
	due := memo.due(interval)
	if now.Before(due) || memo.unrestorable {
		c.saves.mu.Unlock()
		return nil
	}
	select {
	case c.saves.slots <- struct{}{}:
	default:
		c.saves.mu.Unlock()
		return nil
	}
	running := &saveRun{taken: make(chan struct{}), ended: make(chan struct{})}
	memo.running = running
	job := saveJob{run: run, due: due, mark: memo.mark, interval: interval, last: memo.last,
		counted: memo.counted, taken: running.taken}
	c.saves.mu.Unlock()
	return c.spawnSave(offerer, job, memo, running)
}

// spawnSave starts job's task under offerer, counted in c.inflight, unless the
// coordinator has stopped: then it gives back the slot its caller took and
// clears memo's task, and answers nil.
func (c *Coordinator) spawnSave(offerer context.Context, job saveJob, memo *saveMemo, running *saveRun) *saveRun {
	c.mu.Lock()
	closed := c.closed
	if !closed {
		c.inflight.Add(1)
		go c.runSave(offerer, job, memo, running)
	}
	c.mu.Unlock()
	if closed {
		c.saves.mu.Lock()
		memo.running = nil
		c.saves.mu.Unlock()
		<-c.saves.slots
		return nil
	}
	return running
}

// SaveHosted saves the session of every running Direct job this node
// launched whose session moved since its last save here, most stale first,
// under the node's save slots, for at most [drainSaveWait]; from its first
// instant the node takes no periodic save. It returns once each save it
// picked has passed its spool and joined the keeps, or at its wait; their
// keeps go on, counted with every keep, until the coordinator's stop (the
// package doc's "A save as its node drains").
func (c *Coordinator) SaveHosted(ctx context.Context) {
	c.saveHosted(ctx)
}

// saveHosted is [Coordinator.SaveHosted], answering every task it waited on.
func (c *Coordinator) saveHosted(ctx context.Context) []*saveRun {
	// RULE 1, saves are on, as an offer's.
	if !c.capturing() || c.taker == nil || c.saves.ledger == nil {
		return nil
	}
	c.saves.mu.Lock()
	c.saves.draining = true
	c.saves.mu.Unlock()
	begun := c.now()
	drainEnd := time.Now().Add(drainSaveWait)
	if deadline, ok := ctx.Deadline(); ok && deadline.Before(drainEnd) {
		drainEnd = deadline
	}
	wait, cancel := c.drainContext(ctx, drainEnd)
	defer cancel()
	var picked []*saveRun
	for _, hosted := range c.hostedSaves(wait) {
		running := hosted.running
		if running == nil {
			running = c.drainSave(wait, hosted.run, begun, drainEnd)
		}
		if running == nil {
			// The wait ended, or the coordinator stopped: no slot comes now.
			break
		}
		picked = append(picked, running)
	}
	for _, running := range picked {
		select {
		case <-running.taken:
		case <-wait.Done():
			return picked
		}
	}
	return picked
}

// drainContext is a drain's wait for its saves: ctx's, ended at drainEnd or
// the coordinator's stop.
func (c *Coordinator) drainContext(ctx context.Context, drainEnd time.Time) (context.Context,
	context.CancelFunc) {

	wait, cancel := context.WithDeadline(ctx, drainEnd)
	stop := context.AfterFunc(c.life, cancel)
	return wait, func() {
		stop()
		cancel()
	}
}

// hostedSave is a job a drain saves: its run as listed, the save of its turn
// already under way on this node (nil for none), and when it was last saved.
type hostedSave struct {
	run     PendingRun
	running *saveRun
	stale   time.Time
}

// hostedSaves is every job a drain saves, most stale first by this node's
// memory of its last save, else by the later of its launch's start and its
// newest capture: running, its ending undecided, its box a Direct one this
// node launched, its harness one that saves, and no save of it here found its
// session one no box continues ([saveUnrestorable]).
func (c *Coordinator) hostedSaves(ctx context.Context) []hostedSave {
	manager := c.mgr()
	if manager == nil {
		return nil
	}
	runs, err := c.pending.ListActive(ctx)
	if err != nil {
		sayNotKept(ctx, slog.LevelWarn, executorkeep.LossKeepFailed, "", "point", drainSavePoint,
			"detail", fmt.Sprintf("this node's runs could not be listed: %v", err))
		return nil
	}
	var hosted []hostedSave
	now := c.now()
	c.saves.mu.Lock()
	for _, run := range runs {
		if run.Status != StatusRunning || run.SandboxID == "" || run.Ending != nil || run.LaunchNode != c.node ||
			manager.CellOf(run) != Direct {
			continue
		}
		runner, _ := manager.RunnerFor(run.Harness)
		if saver, ok := runner.(SessionSaver); !ok || !saver.Saves() {
			continue
		}
		job := hostedSave{run: run, stale: lastSaved(run, now)}
		if memo := c.saves.memos[run.TurnID]; memo != nil {
			if memo.of(run) && memo.unrestorable {
				continue
			}
			job.running = memo.running
			if memo.of(run) {
				job.stale = memo.last
			}
		}
		hosted = append(hosted, job)
	}
	c.saves.mu.Unlock()
	slices.SortFunc(hosted, func(a, b hostedSave) int {
		return cmp.Or(a.stale.Compare(b.stale), cmp.Compare(a.run.TurnID, b.run.TurnID))
	})
	return hosted
}

// drainSave starts run's drain save once a slot is free, waiting for one until
// wait ends, and answers it: the turn's task already under way where one
// started meanwhile, nil where none started.
func (c *Coordinator) drainSave(wait context.Context, run PendingRun, begun, drainEnd time.Time) *saveRun {
	select {
	case c.saves.slots <- struct{}{}:
	case <-wait.Done():
		return nil
	}
	now, interval := c.now(), c.keep.SaveInterval()
	c.saves.mu.Lock()
	if memo := c.saves.memos[run.TurnID]; memo != nil && memo.running != nil {
		c.saves.mu.Unlock()
		<-c.saves.slots
		return memo.running
	}
	memo := c.saves.memoOf(run, now, interval)
	running := &saveRun{taken: make(chan struct{}), ended: make(chan struct{})}
	memo.running = running
	job := saveJob{run: run, drainEnd: drainEnd, due: begun, mark: memo.mark, interval: interval, last: memo.last,
		counted: memo.counted, taken: running.taken}
	c.saves.mu.Unlock()
	// NO OFFERER: a drain's save ends its box phase at drainEnd or the
	// coordinator's stop, never with the wait that picked it.
	return c.spawnSave(context.WithoutCancel(wait), job, memo, running)
}

// runSave runs one save task its offer spawned under offerer, then writes what
// came of it to memo, gives its slot back once its keep has ended, and logs a
// failure no stamp's line names, all before it closes running.ended: a waiter
// on ended may move the clock or the memory on.
func (c *Coordinator) runSave(offerer context.Context, job saveJob, memo *saveMemo, running *saveRun) {
	start := c.now()
	life, phase, endPhase := c.saveContexts(offerer, job.drainEnd)
	out := c.saveTask(life, phase, job)
	endPhase()
	c.saves.mu.Lock()
	memo.running = nil
	if out.counted > 0 {
		memo.counted = out.counted
	}
	switch out.class {
	case saveGone:
		if c.saves.memos[job.run.TurnID] == memo {
			delete(c.saves.memos, job.run.TurnID)
		}
	case savePaced:
		memo.pacedUntil = out.pacedUntil
	case saveNotYet, saveNothing, saveUnresolved:
		// NEVER DOUBLED: what was not there now says nothing of the box.
		memo.last = start
	case saveFailed:
		memo.last = start
		memo.failures++
	case saveUnrestorable:
		memo.last, memo.unrestorable = start, true
	case saveLanded:
		memo.last, memo.failures, memo.mark = start, 0, out.streamed
	}
	failures, nextDue := memo.failures, memo.due(job.interval).Sub(c.now())
	unlogged := out.unplaced != "" && !memo.logged[out.unplaced]
	if unlogged {
		memo.logged[out.unplaced] = true
	}
	c.saves.mu.Unlock()
	<-c.saves.slots
	defer close(running.ended)
	if out.unplaced == "" {
		return
	}
	level := slog.LevelDebug
	if unlogged {
		level = slog.LevelWarn
	}
	loss := out.loss
	if loss == "" {
		loss = executorkeep.LossKeepFailed
	}
	sayNotKept(life, level, loss, job.run.TurnID, "point", job.point(),
		"failures", failures, "next_due_s", nextDue.Seconds(), "detail", out.why)
}

// memoOf is run's memory, made at FIRST SIGHT where none is kept or it counts
// another job or box, and dated to this offer; memories no offer came for in
// [saveRetryCeiling] are forgotten first, at most once in that time. Its
// caller holds s.mu and has checked no task of the turn runs.
func (s *saveState) memoOf(run PendingRun, now time.Time, interval time.Duration) *saveMemo {
	if now.Sub(s.swept) >= saveRetryCeiling {
		for turn, memo := range s.memos {
			if memo.running == nil && now.Sub(memo.offered) >= saveRetryCeiling {
				delete(s.memos, turn)
			}
		}
		s.swept = now
	}
	memo := s.memos[run.TurnID]
	if !memo.of(run) {
		memo = &saveMemo{launch: run.LaunchID, box: run.SandboxID, last: firstSight(run, now, interval),
			mark: -1, logged: map[string]bool{}}
		s.memos[run.TurnID] = memo
	}
	memo.offered = now
	return memo
}

// firstSight is when a job first seen by this node's memory was last saved,
// as far as the run's row can say ([lastSaved]), clamped to within one
// interval of now. RULE H's: an instant another machine stamped only shortens
// a wait one interval bounds (the package doc's "A save is offered by the
// poll, from memory").
func firstSight(run PendingRun, now time.Time, interval time.Duration) time.Time {
	return now.Add(-min(max(now.Sub(lastSaved(run, now)), 0), interval))
}

// lastSaved is the later of run's launch's start and its newest capture of
// this launch, or now where neither is set.
func lastSaved(run PendingRun, now time.Time) time.Time {
	newest := run.Launch.StartedAt
	if capture := run.Capture; capture != nil && capture.Launch == run.LaunchID &&
		capture.LaunchSeq == run.LaunchSeq && capture.At.After(newest) {
		newest = capture.At
	}
	if newest.IsZero() {
		return now
	}
	return newest
}

// due is when the turn's next save is due: its interval after the last
// attempt, doubled for each failure in a row up to the larger of the interval
// and [saveRetryCeiling], and never before the seat's allowance allows it.
func (m *saveMemo) due(interval time.Duration) time.Time {
	ceiling := max(interval, saveRetryCeiling)
	wait := interval
	for range m.failures {
		if wait >= ceiling {
			break
		}
		wait *= 2
	}
	due := m.last.Add(min(wait, ceiling))
	if m.pacedUntil.After(due) {
		return m.pacedUntil
	}
	return due
}
