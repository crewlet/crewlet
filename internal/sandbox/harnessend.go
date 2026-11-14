package sandbox

import (
	"context"
	"encoding/json"
	"slices"
)

// HOW A JOB'S HARNESS ENDED, AND WHAT ITS NEWEST REQUEST CARRIED, ON THE JOB'S
// OWN RECORD.
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
//
// AND WHAT THE COPY'S NEWEST REQUEST CARRIED, on the same record in the same
// write ([LaunchRecord.Context]). A relaunch that continues the copy a job's box
// holds sends that session's whole context again before it reads a word of its
// brief, so its floor is that context with the brief (internal/engine's
// continuedFloor). A capture's stamp measures its archive as it is taken
// ([SessionCapture.ContextTokens]), but a company that keeps no session takes
// none, and a job's newest capture can be missing or unmeasured: such a copy
// was held to the floor with its brief beside it whatever it carried, bounded
// only by its harness's own compaction. So the read that collects a job takes,
// off the run's own account of its requests ([Result.LastRequest]: Claude Code's
// event stream, OpenCode's session message list as the job ends), what the
// newest request of the copy's harness carried, its uncached input, cache reads
// and cache writes, and records it beside the harness's end, with NO READ OF THE
// BOX OF ITS OWN. A job that made no request of its own carries the figure along
// its copy's lineage, and says where it came from ([MeasureSource]), so no
// reader takes a carried figure for a fresh measure of the job: a relaunch that
// continues the copy in place, the record of the job whose copy it is; one that
// continues a kept capture, that capture's own measure; one that continues the
// session the conversation's last turn left, the base's; a fresh one, none.
//
// AND WHAT CLAUDE CODE COUNTS THAT COPY AT before its next request
// ([LaunchRecord.ResumedCount], resumed.go), as the same read found it in one
// read of the job's main thread (mainthread.go), in the same write: never
// carried, the read of this job's copy at its collection or nothing, so the
// next launch's record starts with none.

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

// RequestMeasure is what the newest request of the session a job's box ran
// carried, as the run's own account measured it ([Result.LastRequest]): its
// uncached input, its cache reads and its cache writes, the input a launch that
// continues the session sends again; and whether the job's own run made it,
// rather than an earlier job of the session it took up (an OpenCode session's
// newest request that measured a prompt can be one). Tokens 0 measured none.
type RequestMeasure struct {
	Tokens int64
	Own    bool
}

// MeasureSource is where a job's record's measure of the session its box holds
// came from ([SessionMeasure.Source]).
type MeasureSource string

const (
	// MeasureRequest is a request of the job [SessionMeasure.Launch] names,
	// read as that job ended: a fresh measure of that job's copy.
	MeasureRequest MeasureSource = "request"
	// MeasureCapture is a capture's measure of its archive, taken as it was
	// spooled ([SessionCapture.ContextTokens]); Launch names the capture's job,
	// whose archive it is, never as that job's request.
	MeasureCapture MeasureSource = "capture"
	// MeasureBase is the session the conversation's last turn left, as the
	// turn's frozen base records it ([BaseSession.ContextTokens]).
	MeasureBase MeasureSource = "base"
	// MeasureSession is an earlier request of the session the job took up,
	// read as the job ended, whose job is not known.
	MeasureSession MeasureSource = "session"
)

// MeasureSources are the four.
var MeasureSources = []MeasureSource{MeasureRequest, MeasureCapture, MeasureBase, MeasureSession}

// Valid reports whether this build knows s. A record a later build wrote may
// name a source this build does not know, which is read as the value it is: its
// figure still holds the copy to a floor, and no reader takes it for a fresh
// measure of the job, which only [MeasureRequest] is.
func (s MeasureSource) Valid() bool { return slices.Contains(MeasureSources, s) }

// SessionMeasure is what the session a job's box ran carried at its newest
// request the engine measured, and where that figure came from
// ([LaunchRecord.Context]). Only a Source of [MeasureRequest] whose Launch names
// the record's own job is a fresh measure of the job; every other is carried
// along the copy's lineage and says from where.
type SessionMeasure struct {
	// Tokens is the figure: the request's uncached input, cache reads and
	// cache writes.
	Tokens int64 `json:"tokens"`
	// Source is where it came from.
	Source MeasureSource `json:"source"`
	// Launch is the job whose request measured it, for [MeasureRequest], or
	// whose capture's archive, for [MeasureCapture]; "" for the others.
	Launch string `json:"launch,omitempty"`
	// Extra is every key of the measure this build does not know, kept
	// verbatim and written back (wirekeys.go): a later build's word on where
	// a figure came from survives this build's writes of the job's record.
	Extra map[string]json.RawMessage `json:"-"`
}

// ContextTokens is the figure m holds, 0 where there is none: a record with no
// measure, and a figure that is no count of a session's context.
func (m *SessionMeasure) ContextTokens() int64 {
	if m == nil {
		return 0
	}
	return max(m.Tokens, 0)
}

// same reports whether m and o say the same of a session's measure, a nil one
// none at all.
func (m *SessionMeasure) same(o *SessionMeasure) bool {
	switch {
	case m == nil || o == nil:
		return m == o
	case !m.says(o) || len(m.Extra) != len(o.Extra):
		return false
	}
	for key, value := range m.Extra {
		if string(o.Extra[key]) != string(value) {
			return false
		}
	}
	return true
}

// says reports whether m holds o's figure, where it came from and whose it is,
// whatever keys a later build added beside them ([SessionMeasure.Extra]); a
// nil one says nothing.
func (m *SessionMeasure) says(o *SessionMeasure) bool {
	return m != nil && o != nil && m.Tokens == o.Tokens && m.Source == o.Source && m.Launch == o.Launch
}

// EndRead is what the read that collected a job found of its end: how its
// harness ended, what its newest request carried, and what Claude Code counts
// the copy at ([PendingStore.MarkHarnessEnd]).
type EndRead struct {
	// End is how the job's harness ended ([LaunchRecord.HarnessEnd]).
	End HarnessEnd
	// Request is what the newest request of the session the job's box ran
	// carried, as its run's own account measured it ([Result.LastRequest]).
	Request RequestMeasure
	// Count is what Claude Code counts the copy the job's box holds at before
	// its next request, as the read of the job's main thread found it
	// ([Result.ResumedCount]); nil where the read asked none.
	Count *ResumedCount
}

// counted is the count l, a job's record, holds once a read of the job found
// read: the read's wherever it made one, a mark among them, so a later read of
// the job that could not bound the copy replaces an earlier bound, which tells
// more and never less; and l's own where the read made none (a read that asked
// nothing, or a host of an older build). Held to what a record carries
// ([ResumedCount.bounded]): a mark past its bound, or a restart past a uuid's
// whatever the mark, is [CountUnread].
func (l LaunchRecord) counted(read *ResumedCount) *ResumedCount {
	if read == nil {
		return l.ResumedCount
	}
	return read.bounded()
}

// measured is the measure l, the record of job launch, holds once a read of the
// job found read, the newest request its run's account measured:
//
//   - one the job's own run made: that figure, a request of the job's own;
//   - one an earlier job of the session made (OpenCode's newest request that
//     measured a prompt, its step not among the run's own): the measure l
//     carried where its figure is the read's and the job's box did not say it
//     started a session of its own, which says whose it is, and else that
//     figure, as the session's, whose job is not known;
//   - none, where the job's box did not say it started a session of its own
//     (it continued the one it was handed, never said which, or said it in a
//     word this build does not know): the measure l carried, which is the
//     copy's lineage's, an over-estimate where the box started afresh after
//     all, the safe direction for a floor;
//   - none, where the box said it started a session of its own: none, since
//     what l carried is another session's.
//
// A figure that is no count of a session's context is none.
//
// AND THE RECORD'S OWN MEASURE WHERE IT SAYS THAT ALREADY, its Extra with it:
// a collection made again (a claim handed back after a resume that failed) of
// a job a later build collected first finds the measure that build wrote, with
// its word on where the figure came from, and a measure built afresh would
// drop that word in a write that changes nothing this build knows.
func (l LaunchRecord) measured(read RequestMeasure, launch string) *SessionMeasure {
	var m *SessionMeasure
	switch {
	case read.Tokens > 0 && read.Own:
		m = &SessionMeasure{Tokens: read.Tokens, Source: MeasureRequest, Launch: launch}
	case read.Tokens > 0 && l.SessionStart != SessionStartFresh && l.Context != nil &&
		l.Context.Tokens == read.Tokens:
		return l.Context
	case read.Tokens > 0:
		m = &SessionMeasure{Tokens: read.Tokens, Source: MeasureSession}
	case l.SessionStart == SessionStartFresh:
		return nil
	default:
		return l.Context
	}
	if l.Context.says(m) {
		return l.Context
	}
	return m
}

// markHarnessEnd records on run's job's record how its harness ended, what its
// newest request carried and what Claude Code counts the copy at, as the read
// that collects the job found them ([PendingStore.MarkHarnessEnd]), in ONE
// WRITE: neither the measure nor the count adds a write of its own.
//
// UNDER A CONTEXT OF ITS OWN ([stampContext]), as the take's own records are:
// the record is owed to a relaunch that may come days later, on another node —
// a person's answer — whatever becomes of this read's context. AND IN THE ROOM
// THE READ LEAVES THOSE RECORDS ([recordsBy]), after the take: the read's budget
// ([BoxReadBudget]) holds no time of its own for this write, so made before the
// take, a store slow to answer it cut the take's time, and made past that room,
// the box's pause and the read's answer. A record that does not land is said,
// and the job then reads as one whose harness may have been cut off: a relaunch
// that reads the record later carries its round on in its box telling the
// session there what such a copy may lack, which it may already hold, and that
// longer brief counts toward the launch's floor, beside the copy's context
// where its record carries a measure of it and beside the static floor where
// it carries none (internal/engine's continuedFloor). The claim this read
// collects for is told what the read found whether or not the record landed
// ([Coordinator.collectClaimed]).
func (c *Coordinator) markHarnessEnd(ctx context.Context, run PendingRun, read EndRead) {
	markCtx, cancel := stampContext(ctx, recordsBy(ctx))
	defer cancel()
	if _, err := c.pending.MarkHarnessEnd(markCtx, run.TurnID, run.LaunchID, read); err != nil {
		log.WarnContext(ctx, "sandbox_harness_end_unrecorded", "turn_id", run.TurnID,
			"launch_id", run.LaunchID, "harness_end", string(read.End),
			"request_tokens", read.Request.Tokens, "error", err.Error(),
			"detail", "the run's record does not say how its job's harness ended, what the session's "+
				"newest request carried, nor what Claude Code counts it at: the coordination store refused "+
				"the write, or did not answer it "+
				"within the room the read leaves its records. A relaunch that reads the record later, such as "+
				"a person's answer, reads the job as one whose harness may have been cut off, and tells the "+
				"session in its box every call of the round and every note the turn has read, some of which it "+
				"may hold, its floor held to the measure the job's record carried along its copy's lineage, or "+
				"to none; a replaced sign-in carried on from this read's own collection reads what this read "+
				"found")
	}
}
