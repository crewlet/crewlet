package sandbox

import (
	"context"
	"encoding/json"
	"slices"
)

// HOW A JOB'S HARNESS ENDED, AND WHAT ITS NEWEST REQUEST CARRIED, ON THE JOB'S
// OWN RECORD. A relaunch carrying a round on in the job's own box
// ([LaunchRequest.Continue]) continues the copy that box holds, which no
// capture's stamp describes and which holds every call the job made only where
// its harness ended its own run ([readCapture]): Claude Code 2.1.310 can have a
// call answered before it writes it, a job ended before its harness started
// never read its brief, and a continued session's brief is not known written.
// So the read collecting a job ([Coordinator.readBox]) records the end
// ([PendingStore.MarkHarnessEnd]) and carries it to its claim, read before that
// record ([Result.HarnessEnd]); a relaunch not finding its own run there tells
// the session it was, or may have been, cut off, with every call of the round
// and note the turn read (internal/agent/runner's Resume.HarnessEnd).

// On the job's record because the fact is the job's: the turn's row outlives
// it, and [PendingStore.BeginLaunch] replaces the record only after the
// relaunch reading it wrote its briefs. A read made while the run's ending is
// decided (a stop's, [Coordinator.keepLostSession]) records nothing, since no
// relaunch carries that round on; [Coordinator.carryOnLost]'s collection
// records as any does. The same write records [LaunchRecord.Context], what the
// copy's newest request carried ([Result.LastRequest], with no read of the
// box), the floor of a relaunch continuing the copy (internal/engine's
// continuedFloor) where no capture measured it; a job that made no request
// carries its copy's lineage's figure and says from where ([MeasureSource]).
// And [LaunchRecord.ResumedCount], the harness's count of the copy
// (resumed.go), never carried, so the next launch's record starts with none.

// Off the same read of the copy's main thread (mainthread.go), the same write
// records [LaunchRecord.NextRequestTokens], the harness's own estimate of the
// copy's next request, which a relaunch continuing the copy in place is held
// to with its brief (package doc); never carried either.

// HarnessEnd is how a job's harness ended, as the read that collected the job
// found it ([LaunchRecord.HarnessEnd]).
type HarnessEnd string

const (
	// HarnessEndOwnRun is a harness that ended its own run: the job over, its
	// harness gone cleanly by itself, its script packing its session. Its box's
	// copy holds every call it made, one it gave up on as timed out
	// ([BridgeCall.Abandoned]).
	HarnessEndOwnRun HarnessEnd = "own_run"
	// HarnessEndCutOff is every other end ([readCapture]): a harness killed
	// (past its grace, by its memory limit, by a crash), told to stop, or gone
	// with a nonzero status ([Result.HarnessCutOff]), a vendor refusing a
	// replaced sign-in among them; a job ended before its harness started; a box
	// that packed no session; and a job still working once its end was done.
	HarnessEndCutOff HarnessEnd = "cut_off"
)

// HarnessEnds are the two.
var HarnessEnds = []HarnessEnd{HarnessEndOwnRun, HarnessEndCutOff}

// Valid reports whether this build knows e.
func (e HarnessEnd) Valid() bool { return slices.Contains(HarnessEnds, e) }

// EndedOwnRun reports whether e says the job's harness ended its own run. An
// unrecorded end (an older build's job, one no collection read, a write the
// store refused) or a word this build does not know says no: the safe
// direction, since a copy said to be whole is told none of the calls it may lack.
func (e HarnessEnd) EndedOwnRun() bool { return e == HarnessEndOwnRun }

// after is what a job's record holds once a read of the job finds read, where
// it held e. CUT OFF ONCE, CUT OFF FOR THE JOB: a cut-off read replaces an own
// run, never the reverse, nor a word a later build may have written; a read
// that says nothing changes nothing. So no later read says whole a copy an
// earlier one found cut off.
func (e HarnessEnd) after(read HarnessEnd) HarnessEnd {
	if !read.Valid() || (e != "" && e != HarnessEndOwnRun) {
		return e
	}
	return read
}

// harnessEnd is how the job's harness ended as this capture point found it:
// the end and the save point are drawn by one line.
func (p capturePoint) harnessEnd() HarnessEnd {
	if p.cutOff {
		return HarnessEndCutOff
	}
	return HarnessEndOwnRun
}

// RequestMeasure is what the newest request of the session a job's box ran
// carried, as the run's own account measured it ([Result.LastRequest]): its
// uncached input, cache reads and cache writes, which a launch continuing the
// session sends again. Own says the job's own run made it, not an earlier job
// of the session (as OpenCode's newest prompt-measuring request can be).
// Tokens 0 measured none.
type RequestMeasure struct {
	Tokens int64
	Own    bool
}

// NextRequest is the harness's own estimate of the NEXT request a session
// sends, before a word of the brief that continues it: its last counted
// reply's input, cache and output, and its estimate of every record after
// that reply, restarted at a compaction (internal/sandbox/executor's
// nextReading). A continuing launch is held to it with its brief
// (internal/engine's continuedFloor) rather than to the last request's input
// ([RequestMeasure]), which under-counts the reply and what followed and
// over-counts past a compaction. Tokens at three characters a token,
// OlderTokens at four ([ClaudeTextRate]; 0 where the harness counts one
// rate); zero is no figure. Never carried and never on the executor log: the
// package doc ("the harness's own estimate of a session's next request").
type NextRequest struct {
	Tokens      int64
	OlderTokens int64
}

// At is n at model's rate ([ClaudeTextRate]): OlderTokens for a model Claude
// Code counts at four characters a token where n holds that figure, else
// Tokens, which is the larger, so a model this build cannot place is held to
// more and never less. Zero where n is no figure, a figure below zero among
// it.
func (n NextRequest) At(model string) int64 {
	if n.OlderTokens > 0 && ClaudeTextRate(model) == claudeOlderTextRate {
		return n.OlderTokens
	}
	return max(n.Tokens, 0)
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

// Valid reports whether this build knows s. A source a later build wrote is
// kept as it is: its figure still holds the copy to a floor, and only
// [MeasureRequest] is a fresh measure of the job.
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

// ContextTokens is the figure m holds, 0 where there is none: no measure, or a
// negative figure.
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
	// CountProvider is the provider of the reply an OpenCode Count is
	// anchored on, off the same read ([Result.CountProvider]); "" for a Claude
	// Code count and where no reply anchored one.
	CountProvider string
	// Next is the harness's own estimate of the copy's next request, off the
	// same read ([Result.NextRequest]); zero where the read made none.
	Next NextRequest
}

// NextRequest is what l, a job's record, says the copy its box holds sends
// next ([LaunchRecord.NextRequestTokens]).
func (l LaunchRecord) NextRequest() NextRequest {
	return NextRequest{Tokens: l.NextRequestTokens, OlderTokens: l.NextRequestOlderTokens}
}

// setNextRequest writes n onto l, both rates at once.
func (l *LaunchRecord) setNextRequest(n NextRequest) {
	l.NextRequestTokens, l.NextRequestOlderTokens = n.Tokens, n.OlderTokens
}

// nextRequested is the estimate l, a job's record, holds once a read of the
// job found read: the read's where it made one, and l's own where it made none
// (a read that asked nothing, a thread it could not read whole, a host of an
// older build). The job's copy does not change once the job is over, and only
// a read of a job that is over reads its thread, so an earlier read's figure
// still describes the copy a later read could not: kept, a relaunch is held
// to it rather than to the copy's newest input, which under-counts it.
func (l LaunchRecord) nextRequested(read NextRequest) NextRequest {
	if read.Tokens > 0 {
		return read
	}
	return l.NextRequest()
}

// counted is the count l, a job's record, holds once a read of the job found
// read, with the provider of the reply it is anchored on
// ([LaunchRecord.CountProvider]): the read's wherever it made one, a mark among
// them, since a later read that could not bound the copy tells more than an
// earlier bound; l's own where it made none (it asked nothing, or an older
// build's host). The provider is written and cleared with the count, so no
// record names one count's provider beside another's figure. Held to what a
// record carries ([ResumedCount.bounded]); a count beside a provider no record
// may carry ([ValidCountProvider]) is [CountUnread] with none.
func (l LaunchRecord) counted(read *ResumedCount, provider string) (*ResumedCount, string) {
	if read == nil {
		return l.ResumedCount, l.CountProvider
	}
	return read.provided(provider)
}

// measured is the measure l, the record of job launch, holds once a read of the
// job found read:
//   - the job's own request: that figure, as the job's;
//   - an earlier job's (OpenCode's newest prompt-measuring request): l's own
//     where it holds that figure and the box did not say it started afresh,
//     which says whose it is, else the figure as the session's;
//   - none: l's, the lineage's, unless the box said it started afresh, an
//     over-estimate at worst, the safe direction for a floor.
//
// A non-positive figure is none. Where l already says the same, l's own is
// kept, Extra with it: a collection made again of a job a later build collected
// first must not drop that build's word in a write that changes nothing here.
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
// newest request carried, what its harness counts the copy at and estimates its
// next request at ([PendingStore.MarkHarnessEnd]), in ONE write. Under a
// context of its own ([Coordinator.recordContext]), since a relaunch may read
// it days later on another node; after the take, in the room the read leaves
// its records ([Coordinator.recordsBy], [Coordinator.takeRecordsRoom],
// [BoxReadBudget]): made before the take, a slow store would cut the take's
// time, and past that room, the box's pause and the read's answer. A record
// that does not land is logged, and a later relaunch reads the harness as maybe
// cut off, telling its session what it may already hold in a longer brief
// counted toward its floor (internal/engine's continuedFloor); the claim is
// told what this read found either way ([Coordinator.collectClaimed]).
func (c *Coordinator) markHarnessEnd(ctx context.Context, run PendingRun, read EndRead) {
	markCtx, cancel := c.recordContext(ctx, c.recordsBy(ctx))
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
