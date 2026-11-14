package sandbox

import (
	"context"
	"slices"
)

// HOW A JOB'S HARNESS ENDED, ON THE JOB'S OWN RECORD.
//
// A relaunch that carries a round on IN THE JOB'S OWN BOX — a person's answer to
// a run that parked, or a sign-in replaced while it ran, where the relaunch
// takes up no kept capture of the job ([LaunchRequest.Continue]) — continues the
// copy of the session that box holds, and no capture's stamp tells it what that
// copy is: a company that keeps no session takes none, and a capture the
// relaunch does not continue — its keep still under way past the relaunch's
// wait, or refused — is one the relaunch does not read. That copy holds every
// call the job made — of one its harness gave up on, only that it timed out
// ([BridgeCall.Abandoned]) — only where its harness ENDED ITS OWN RUN — the
// job over, its harness gone cleanly by itself, its script packing it — by the
// line a
// capture's save point is drawn by ([readCapture], savepoint.go): a harness can
// make a call, and have it answered, before it writes the call to its record
// (measured on Claude Code 2.1.310), so one killed, crashed, told to stop or
// ended on an error can leave a copy that lacks calls it made and had answered,
// and one whose job ended before it started holds the session it was handed,
// which never read the job's brief. Nor is a copy known to hold that brief: no
// probe has measured whether 2.1.310 writes the message a continued session is
// handed before it answers a call, and a copy whose job continued a session and
// lacks it lacks everything that brief told. (Its first calls were measured
// answered before a FRESH session's transcript file existed; a fresh session cut
// off then leaves its box nothing to continue, and the relaunch starts a
// session of its own.)
//
// So the read that collects a job ([Coordinator.readBox]) records how its
// harness ended on the job's own record ([LaunchRecord.HarnessEnd],
// [PendingStore.MarkHarnessEnd]), whether or not it takes or keeps a capture of
// the session, and carries it back with the job's result ([Result.HarnessEnd])
// for the claim it collects for, which was read before the record was written.
// Unless the record says its harness ended its own run (internal/agent/runner's
// Resume.HarnessEnd), a relaunch that carries the round on in the job's box
// tells the session there that it was cut off — or, where nothing was
// recorded, that it may have been — every call of the round and every note
// the turn has read.
//
// THE JOB'S RECORD AND NO OTHER, because the fact is the job's: the turn's row
// outlives each job on it, a capture's stamp exists only where a capture was
// taken, and the job's record is replaced whole by the next launch
// ([PendingStore.BeginLaunch]), which is after the relaunch that reads it has
// written its briefs. A read made while the run's ending is decided — a stop's,
// and the read a loss's ending makes ([Coordinator.keepLostSession]) — records
// nothing: no relaunch carries that run's round on. A lost job whose round IS
// carried on is read by a collection ([Coordinator.carryOnLost]), which records
// it as any collection does.

// HarnessEnd is how a job's harness ended, as the read that collected the job
// found it ([LaunchRecord.HarnessEnd]).
type HarnessEnd string

const (
	// HarnessEndOwnRun is a harness that ended its own run: the job over, its
	// harness gone cleanly by itself, its script packing its session. The copy
	// its box holds has every call it made, and the outcome of each but one it
	// gave up on ([BridgeCall.Abandoned]), which it holds as timed out.
	HarnessEndOwnRun HarnessEnd = "own_run"
	// HarnessEndCutOff is every other end ([readCapture]): a harness killed —
	// past its end's grace, by its memory limit, by a crash — told to stop, or
	// gone with any status but 0 ([Result.HarnessCutOff]), as one that exits on
	// an error is, its vendor's refusal of a replaced sign-in among them where
	// it exits so; one whose job ended before it started, one whose box packed
	// no session, and a job still working once its end was done.
	HarnessEndCutOff HarnessEnd = "cut_off"
)

// HarnessEnds are the two.
var HarnessEnds = []HarnessEnd{HarnessEndOwnRun, HarnessEndCutOff}

// Valid reports whether this build knows e.
func (e HarnessEnd) Valid() bool { return slices.Contains(HarnessEnds, e) }

// EndedOwnRun reports whether e says the job's harness ended its own run. An
// end nobody recorded — a job an older build collected, one no collection read,
// a record the coordination store did not take — says no, and so does a word
// this build does not know: the safe direction, since a copy said to be whole is
// told none of the calls it may lack.
func (e HarnessEnd) EndedOwnRun() bool { return e == HarnessEndOwnRun }

// after is what a job's record holds once a read of the job finds read, where
// it held e. CUT OFF ONCE, CUT OFF FOR THE JOB: a read that finds the harness
// cut off is recorded over one that found it ended its own run, and never the
// reverse, nor over a word this build does not know, which a later build may
// have written for an end of its own; a read that says nothing changes nothing.
// The safe direction again: a later read never says whole a copy an earlier one
// found cut off.
func (e HarnessEnd) after(read HarnessEnd) HarnessEnd {
	if !read.Valid() || (e != "" && e != HarnessEndOwnRun) {
		return e
	}
	return read
}

// harnessEnd is how the job's harness ended as this capture point found it: its
// own run where the point is not cut off, by the one line both are drawn by.
func (p capturePoint) harnessEnd() HarnessEnd {
	if p.cutOff {
		return HarnessEndCutOff
	}
	return HarnessEndOwnRun
}

// markHarnessEnd records on run's job's record how its harness ended, as the
// read that collects the job found it ([PendingStore.MarkHarnessEnd]).
//
// UNDER A CONTEXT OF ITS OWN ([Coordinator.recordContext]), as the take's own
// records are, within a teardown's grace and the time the run's row takes to
// cross to the store: the record is owed to a relaunch that may come days
// later, on another node — a person's answer — whatever becomes of this read's
// context. AND IN THE ROOM THE READ LEAVES THOSE RECORDS
// ([Coordinator.recordsBy], [Coordinator.takeRecordsRoom]), after the take,
// which counts this write's crossing as the read's budget does
// ([BoxReadBudget]): made before the take, a store slow to answer it cut the
// take's time, and made past that room, the box's pause and the read's answer.
// A record that does not land is said, and the job then reads as one whose
// harness may have been cut off: a relaunch
// that reads the record later carries its round on in its box telling the
// session there what such a copy may lack, which it may already hold, and that
// longer brief counts toward the launch's floor where a capture measured the
// copy's context (internal/engine's continuedFloor). The claim this read collects
// for is told what the read found whether or not the record landed
// ([Coordinator.collectClaimed]).
func (c *Coordinator) markHarnessEnd(ctx context.Context, run PendingRun, end HarnessEnd) {
	markCtx, cancel := c.recordContext(ctx, c.recordsBy(ctx))
	defer cancel()
	if _, err := c.pending.MarkHarnessEnd(markCtx, run.TurnID, run.LaunchID, end); err != nil {
		log.WarnContext(ctx, "sandbox_harness_end_unrecorded", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "harness_end", string(end), "error", err.Error(),
			"detail", "the run's record does not say how its job's harness ended: the coordination store "+
				"refused the write, or did not answer it within the room the read leaves its records. A "+
				"relaunch that reads the record later, such as a person's answer, reads the job as one whose "+
				"harness may have been cut off, and tells the session in its box every call of the round and "+
				"every note the turn has read, some of which it may hold; a replaced sign-in carried on from "+
				"this read's own collection reads what this read found")
	}
}
