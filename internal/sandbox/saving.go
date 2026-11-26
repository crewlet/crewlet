package sandbox

import (
	"fmt"
	"time"
)

// A save under way, and its license: the argument is the package doc's,
// "A save under way" and "A save's license".

// savingTake is the row's save under way as the one rule reads it: its
// [PendingRun.Saving], but nil where the row's newest capture is the same take
// ([SessionCapture.sameTake]), in any state, since every reader then sees the
// save through [PendingRun.Capture], and nil where the row keeps a capture at
// or past its place ([PendingRun.Kept]), since its landing can then change
// nothing. THE ONE RAW READ OF THE FIELD by a reader (the package doc's "A
// save under way").
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
// completion as announced, whatever its status says: a FINAL capture of this
// launch as its newest or kept capture (nothing of a job is taken once a take
// found it ended, [Coordinator.takeCapture]), how its harness ended
// ([LaunchRecord.HarnessEnd]), its turn's wall-clock cap or its turn cap seen
// ending it ([LaunchRecord.EndedAtDeadline], [LaunchRecord.EndedAtTurnCap]), its box read back
// ([LaunchRecord.Collected]), a collection whose read failed
// ([LaunchRecord.CollectFailures]), a refused delivery of its completion owed
// its re-fire ([LaunchRecord.RefusalOwed]), or its loss ([PendingRun.Lost]).
// Every arm is the launch's own: the next launch clears each, and a claim
// handed back leaves the job over, since one that clears its failures
// ([Release.BoxAnswered]) writes how its harness ended, or is a lost job's.
func (r PendingRun) jobOver() bool {
	finalOfThisLaunch := func(c *SessionCapture) bool {
		return c != nil && c.Final && c.Launch == r.LaunchID && c.LaunchSeq == r.LaunchSeq
	}
	job := r.Launch
	return finalOfThisLaunch(r.Capture) || finalOfThisLaunch(r.Kept) || job.HarnessEnd != "" ||
		job.EndedAtDeadline || job.EndedAtTurnCap || job.Collected || job.CollectFailures > 0 || job.RefusalOwed != "" || r.Lost != nil
}

// licensesSave is why run's row refuses save's reservation as of now, empty
// where it licenses it ([PendingStore.ReserveSave]; the package doc's "A
// save's license"): each conjunct a fact the row states, decided on the row
// as the store finds it.
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
