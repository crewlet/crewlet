package sandbox

import (
	"fmt"
	"time"
)

// # A save under way
//
// A SAVE is a capture of a job STILL WORKING, taken while its harness runs on
// ([SessionCapture.Final] false, [SessionCapture.Interrupted] true): its place
// is reserved on the run's row like every capture's, after the row's newest, and
// stamped there as it comes out. What makes it more than a take is that the job
// goes on, and so do the reads of it: the job's collection, a stop's read or a
// loss's take the job's FINAL capture in the place after the save while the
// save may still be uploading, and every stamp of that final take replaces the
// row's newest capture ([PendingRun.Capture]). Read off that section alone, a
// save the final overtook is out of sight while it may still become the
// turn's kept capture: where the final is refused, the save is the newest
// session of the job anybody kept, and an end that recorded over it, waiting
// on the final alone, left its record to meet the end's tombstone.
//
// SO A SAVE KEEPS A SECOND PLACE ON THE ROW, [PendingRun.Saving], from its
// reservation ([PendingStore.ReserveSave]) until it can no longer become the
// turn's kept capture: it lands or is refused, or a capture at or past its
// place is kept ([PendingRun.Kept]), so its landing could change nothing. Every
// stamp of the save's own take moves it on there, and every stamp that takes
// lets it go once that holds ([stampOnto], [savingAfter]). A READING, NOT A ROW
// INVARIANT: a build without the field carries it in [PendingRun.Extra] and
// writes it back unchanged, so a row may hold a stale save: one that build
// settled or asked again in [PendingRun.Capture], or one whose evidence its
// final take overwrote. So nothing reads the field but through one rule
// ([PendingRun.savingTake], [PendingRun.saving]), which a source walk holds
// every reader to, and the next stamp that takes reconciles it from the row as
// that stamp found it.
//
// # The license
//
// A save takes its place ONLY WHILE ITS JOB RUNS UNDECIDED ([licensesSave]),
// decided in one read-decide-write of the row as it stands: the run running,
// the save of its current launch, nothing of the job's end read and no refusal
// of its completion owed ([PendingRun.jobOver]), no loss marked, no ending
// decided, no capture or save of the turn that may still land
// ([PendingRun.capturesMayLand]), and at the place after the row's newest.
// Every read that takes a job's final capture writes its decision on the row
// before it reads the row for its place (the claim, a stop's ending, a loss),
// so a final take either finds the save's place reserved and takes the one
// after it, or the save is refused: A FINAL TAKE NEVER LOSES ITS PLACE TO A
// SAVE.
//
// THE PLACE ORDER IS THE CONTENT ORDER. A save's place, its save point and its
// note cursor come from ONE read of the row made before its pack, so any
// capture placed since that read moves the next place and refuses the save:
// read after the pack, a save packed before a final take could be handed the
// save point that final take's own read placed after the job's end, counting
// calls the save never held. The store holds a save that reads late to the
// job's end besides ([PendingRun.jobOver]).
//
// WHAT THE ARMS LEAVE. A save of a job whose completion is announced but not
// yet recorded on its row (on its way to the seat's holder, claimed and handed
// back before its collection heard from the box's host, or refused on a
// condition the holder hands on, [SeatLost] or [Draining], which nothing
// records) is licensed as an ordinary take of a job that may have ended: the
// collection's final take is placed after it and supersedes it, and where that
// take is not kept, the save is kept holding all the job did, told as a session
// its run went on past. It over-tells; it never holds a call the session did
// not see.

// savingTake is the row's save under way as the one rule reads it: its
// [PendingRun.Saving], but nil where the row's newest capture is the same take
// ([SessionCapture.sameTake]), in any state, since every reader then sees the
// save through [PendingRun.Capture], and nil where the row keeps a capture at
// or past its place ([PendingRun.Kept]), since its landing can then change
// nothing. THE ONE RAW READ OF THE FIELD by a reader (see the file comment).
func (r PendingRun) savingTake() *SessionCapture {
	save := r.Saving
	switch {
	case save == nil:
		return nil
	case r.Capture != nil && save.sameTake(*r.Capture):
		return nil
	case r.Kept != nil && !save.newerThan(*r.Kept):
		return nil
	}
	return save
}

// saving is the row's save under way that may still become the turn's kept
// capture as of now ([SessionCapture.mayLand]), nil where there is none.
func (r PendingRun) saving(now time.Time) *SessionCapture {
	if save := r.savingTake(); save != nil && save.mayLand(now) {
		return save
	}
	return nil
}

// capturesMayLand reports whether a capture or a save of the turn may still
// land as of now: the row's newest capture, or its save under way
// ([SessionCapture.mayLand]). One save of a run is in flight at a time, fleet
// wide: a save is licensed only where this is false ([licensesSave]).
//
// THE NEWEST WHATEVER ITS LAUNCH: a relaunch may go on while the job before
// it is still having its final take kept, and a save of the new launch placed
// over that take would take the newest place from it, where no wait reads it
// any more, so a turn's end could tombstone the turn ahead of its record.
func (r PendingRun) capturesMayLand(now time.Time) bool {
	return (r.Capture != nil && r.Capture.mayLand(now)) || r.saving(now) != nil
}

// jobOver reports whether the row records its current job as over, or its
// completion as announced, whatever its status says: any of
//
//   - a FINAL capture of this launch, its job and its ordinal both the row's,
//     as its newest or as its kept capture: the rule a take of a job reads
//     ([Coordinator.takeCapture]): nothing of a job is taken once a take found
//     it ended;
//   - how its harness ended ([LaunchRecord.HarnessEnd]), which a collection's
//     read writes whether or not it took a capture;
//   - its turn's cap seen ending it ([LaunchRecord.EndedAtDeadline]);
//   - its box read back by a collection ([LaunchRecord.Collected]);
//   - a collection claimed whose read failed ([LaunchRecord.CollectFailures]);
//   - a delivery of its completion refused, acknowledged and owed its re-fire
//     ([LaunchRecord.RefusalOwed]): the completion was announced, and a refusal
//     made before any claim leaves this alone on a running row.
//
// EVERY ARM IS THE LAUNCH'S OWN: the next launch replaces the job's record
// whole, and a capture names its launch by its job and its ordinal. A claim
// handed back puts the row back to running with none of them undone.
func (r PendingRun) jobOver() bool {
	finalOfThisLaunch := func(c *SessionCapture) bool {
		return c != nil && c.Final && c.Launch == r.LaunchID && c.LaunchSeq == r.LaunchSeq
	}
	job := r.Launch
	return finalOfThisLaunch(r.Capture) || finalOfThisLaunch(r.Kept) || job.HarnessEnd != "" ||
		job.EndedAtDeadline || job.Collected || job.CollectFailures > 0 || job.RefusalOwed != ""
}

// licensesSave is why run's row refuses save's reservation as of now, empty
// where it licenses it ([PendingStore.ReserveSave], the file comment): each
// conjunct a fact the row states, decided on the row as the store finds it.
func licensesSave(run PendingRun, save SessionCapture, now time.Time) string {
	switch {
	case run.Status != StatusRunning:
		// A CLAIM of its completion, a park, or any other status: the job's
		// end is being read, or it is not running at all.
		return fmt.Sprintf("the run is %s, not running", run.Status)
	case run.LaunchID != save.Launch || run.LaunchSeq != save.LaunchSeq:
		return fmt.Sprintf("the save is of job %s, launch %d, and the run's job is %s, launch %d", save.Launch,
			save.LaunchSeq, run.LaunchID, run.LaunchSeq)
	case run.jobOver():
		return "the run's record says its job is over, or that its completion was announced"
	case run.Lost != nil:
		return "a loss of the run's job is marked"
	case run.endDecided():
		return "the turn's ending is decided"
	case run.capturesMayLand(now):
		return "a capture or a save of the turn may still land"
	case save.CaptureSeq != nextCaptureSeq(run):
		// A PLACE TAKEN SINCE THE SAVE'S ROW READ, or one it never read: its
		// save point and note cursor are of that read.
		return fmt.Sprintf("the save is placed at %d of its launch, and the next place is %d", save.CaptureSeq,
			nextCaptureSeq(run))
	}
	return ""
}

// reservable is why save cannot be reserved on run turnID's row, nil where it
// can: a capture any stamp may write ([stampable]), and a save's stamp as a
// save is taken: begun, of a job still working, cut off, holding no note,
// stating its save point ([SessionCapture.Through]) and naming no read standing
// by, which only [PendingStore.StandBy] writes. Each is what a save is: the
// store holds its taker to it rather than a comment.
func reservable(turnID string, save SessionCapture) error {
	if err := stampable(turnID, save); err != nil {
		return err
	}
	switch {
	case save.State != CaptureBegun:
		return fmt.Errorf("sandbox: a save of run %s is reserved %s, and a reservation is begun", turnID,
			save.State)
	case save.Final:
		return fmt.Errorf("sandbox: a save of run %s is final, and a save is of a job still working", turnID)
	case !save.Interrupted:
		return fmt.Errorf("sandbox: a save of run %s is not cut off, and a save packs a harness still running",
			turnID)
	case save.NoteCursor != 0:
		return fmt.Errorf("sandbox: a save of run %s holds notes through %d, and a session cut off holds none",
			turnID, save.NoteCursor)
	case save.Through == nil:
		return fmt.Errorf("sandbox: a save of run %s states no save point, and every save is placed at one",
			turnID)
	case save.StandbyTaker != "" || save.StandbyTakerEpoch != 0 || !save.StandbyAt.IsZero():
		return fmt.Errorf("sandbox: a save of run %s names a read standing by for it, which only that read's "+
			"own write names", turnID)
	}
	return nil
}

// holdsSave reports whether run's row already holds save's reservation: its
// save under way, or its newest capture, is the same take
// ([SessionCapture.sameTake]): what a reservation the store applied and
// answered too late for its asker to hear leaves there.
func holdsSave(run PendingRun, save SessionCapture) bool {
	return (run.Saving != nil && save.sameTake(*run.Saving)) || (run.Capture != nil && save.sameTake(*run.Capture))
}

// savingAfter is the save under way run's row keeps once a stamp has TAKEN,
// decided on found, the row's newest capture as that stamp found it: the save
// reconciled from found where found is the save's own take further along (a
// settle or an ask a build without the field wrote there), and then let go
// where it landed or was refused, or where the row keeps a capture at or past
// its place.
//
// ON THE ROW AS FOUND, never as the stamp left it: a final take that overtakes
// a save an older build settled in the newest place replaces the evidence of
// that settle, and decided after it the save would read as still in flight.
func savingAfter(found *SessionCapture, run PendingRun) *SessionCapture {
	save := run.Saving
	if save == nil {
		return nil
	}
	if found != nil && found.sameTake(*save) && save.State.advancesTo(found.State) {
		reconciled := *found
		reconciled.StandbyTaker, reconciled.StandbyTakerEpoch, reconciled.StandbyAt = "", 0, time.Time{}
		save = &reconciled
	}
	if save.State == CaptureLanded || save.State == CaptureRefused || (run.Kept != nil && !save.newerThan(*run.Kept)) {
		return nil
	}
	return save
}
