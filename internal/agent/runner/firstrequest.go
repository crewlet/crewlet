package runner

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// WHETHER A CONTINUED SESSION REACHES ITS RUN'S FIRST REQUEST WHOLE. A relaunch
// tells it what it lacks and counts the rest as held, which can be gone: Claude
// Code may compact it before that request (internal/sandbox's SendsWhole), and a
// session its job restarted after its brief holds only what the restart left. So
// each path decides ONE VALUE, once ([sessionHolds]), after its briefs and lead
// exist, since compaction is judged on the held telling's brief ([firstRequest]);
// every telling of the path and its guard read it. One that may lack what it is
// counted as holding is told as cut off, its guard arming no skill a summary may
// hold only in words. A thread that restarted at all holds the closed rounds only
// in a summary, which no listed call's value can be compared with
// ([sessionHolds.closedSummarized], [Runner.otherCalls]). A kept cut-off session
// may also get a telling by a reading of the calls it holds ([keptTellings]).

// firstRequest is how a relaunch judges whether the session a path continues
// still holds, at its run's first request, what its telling counts as held:
// Claude Code may compact it there, leaving the model a summary
// (internal/sandbox's SendsWhole).
type firstRequest struct {
	// harness is the harness that wrote the session, the only one that
	// continues it.
	harness string
	// window is what the session's harness may hold the run to: the least
	// standard window of the chain's Claude Code members
	// ([SessionBasis.Window]).
	window sandbox.ExecutorWindow
	// count is what Claude Code counts the session at, nil where nothing
	// counted it.
	count *sandbox.ResumedCount
}

// firstRequestVerdict is what a path's first request is known to do before any
// brief is built.
type firstRequestVerdict int

const (
	// byBrief: the brief decides; both tellings are built and the held one's
	// brief is judged ([firstRequest.compacts]); a cut-off capture a reading
	// thins gets a third, judged on its own brief ([keptTellings.led]).
	byBrief firstRequestVerdict = iota
	// heldWhatever: no rule of Claude Code's compacts the session (another
	// harness's): the held telling, whatever the brief.
	heldWhatever
	// compactsWhatever: a count with no bound, an unknown window, or a count
	// alone at or past the point: the compacted telling, whatever the brief.
	compactsWhatever
)

// judges reports whether this build models the compaction rule of the harness
// continuing the session (Claude Code's, internal/sandbox's SendsWhole): THE ONE
// PREDICATE the verdicts and a reading's telling read. A session of a harness it
// does not judge is held whatever its brief and never handed a telling by a
// reading, since nothing here can say its first request hands the model the
// history the reading read (ADR-0073). A newly judged harness is taught here.
func (f firstRequest) judges() bool { return f.harness == sandbox.ClaudeCodeHarness }

// settled is the verdict before any brief is built: held for a harness this
// build does not judge, and compacts for a session that does not send whole
// with no brief at all, since a longer brief never sends whole where a shorter
// one does not.
func (f firstRequest) settled() firstRequestVerdict {
	switch {
	case !f.judges():
		return heldWhatever
	case !f.window.SendsWhole(f.count, ""):
		return compactsWhatever
	}
	return byBrief
}

// compacts is THE VERDICT: whether Claude Code may compact the session before
// it reads brief; false for a harness this build does not judge
// ([firstRequest.judges]). The package's one call of SendsWhole.
func (f firstRequest) compacts(brief string) bool {
	return f.judges() && !f.window.SendsWhole(f.count, brief)
}

// sessionHolds is what a path's tellings may count the continued session as
// holding at its run's first request: decided once per path, after its tellings
// and lead exist, and read by every telling of the path and by its guard.
type sessionHolds struct {
	// cutOff says the record holds the session may lack its job's brief: a
	// capture its harness was cut off in ([SessionBasis.MayLackBrief]), or a
	// box's copy whose job's harness is not known to have ended its own run
	// ([Runner.copyMayLackBrief]).
	cutOff bool
	// restartedInJob says the job's main thread restarted after its brief, or
	// that cannot be read (sandbox.RestartedSince): a resume hands the model
	// nothing before the restart but what it left, so the session may lack the
	// brief and the calls before it. Claude Code's alone, whose thread a count
	// reads; false for another harness's.
	restartedInJob bool
	// compacts says its harness may compact it before its first request
	// ([firstRequest.compacts]).
	compacts bool
	// restarted says the thread restarted before its job or in it, or that
	// cannot be read ([restartedAnywhere]): what it holds from before its
	// newest restart is that restart's summary, or nothing. Claude Code's
	// alone. It decides only how the closed rounds are told
	// ([sessionHolds.closedSummarized]), never the round's calls: an earlier
	// job's restart in this round was followed by a telling of the round from
	// its start, and one in a closed round precedes every call of this one.
	restarted bool
}

// lacksBrief reports whether the session may lack its job's brief: the
// record's word, or a restart in the job.
func (h sessionHolds) lacksBrief() bool { return h.cutOff || h.restartedInJob }

// lacks reports whether the path is told as a cut-off one: every call of its
// round its point does not hold, every note the turn has read, and the leads its
// brief told.
func (h sessionHolds) lacks() bool { return h.lacksBrief() || h.compacts }

// summarized reports whether what the session holds of its work may be a
// summary, or nothing, from some point after its round's start: its harness may
// compact it before its first request, or its job restarted it. It is then told
// every call of the round from its start, every lead its record keeps, and its
// guard arms nothing.
func (h sessionHolds) summarized() bool { return h.restartedInJob || h.compacts }

// closedSummarized reports whether the session may hold the turn's closed
// rounds only in a summary's words: it may be a summary now, or its thread
// restarted, after which the telling repeated only that job's round
// ([sessionHolds.keptCalls]). A restart in an earlier turn's session counts
// too: an over-telling, the safe side.
func (h sessionHolds) closedSummarized() bool { return h.summarized() || h.restarted }

// restartedAnywhere reports whether count finds the session's thread restarted
// at some point, or cannot read whether it did (sandbox.ResumedCount.Restarted).
func restartedAnywhere(count *sandbox.ResumedCount) bool {
	newest, read := count.Restarted()
	return !read || newest != ""
}

// keptCalls is how a kept session of basis is told its round's calls, as h
// decides, in ONE EXPRESSION: where the told calls start, the reads its job's
// brief counted without listing (told first), and whether they are told as
// calls it may already show. Normally the calls after its save point, as calls
// it lacks, or may already show where [sessionHolds.lacks]; where it may be a
// summary ([sessionHolds.summarized]), every call from the round's start,
// owing none. A reading of a cut-off session is asked of these calls
// ([Runner.heldReading], [Runner.calledAfter]).
func (h sessionHolds) keptCalls(basis SessionBasis) (from *int, owed []int, mayShow bool) {
	if h.summarized() {
		return new(0), nil, true
	}
	return &basis.Through, basis.Owed, h.lacks()
}

// keptCount is the count a kept path's verdict is asked of: the capture's own
// where its harness was cut off, since the archive is then always restored over
// the box's copy; else the worse of the capture's and the box copy's (resume's),
// since a box that took the capture whole resumes its own unscrubbed copy,
// which a redaction may have made longer than the archive.
func keptCount(basis SessionBasis, resume *Resume) *sandbox.ResumedCount {
	if basis.Interrupted || resume == nil {
		return basis.Count
	}
	return basis.Count.Worse(resume.Count)
}

// keptRestarted reports whether the job that made basis's capture restarted its
// session after its brief, as the capture's own count finds it against the
// restart that job's launch handed it ([Resume.BasisRestart]); true where
// either cannot be read.
func keptRestarted(basis SessionBasis, resume *Resume) bool {
	if resume == nil {
		return true
	}
	return sandbox.RestartedSince(resume.BasisRestart, basis.Count)
}

// inPlaceRestarted reports whether the job resume collected restarted the copy
// its box holds after its brief, as its collection's count finds it; true where
// either cannot be read.
func inPlaceRestarted(resume *Resume) bool {
	if resume == nil {
		return true
	}
	return sandbox.RestartedSince(resume.BasisRestart, resume.Count)
}

// judged is f's verdict on brief, the held telling's whole brief, where the
// path was left to its brief, logged as executor_session_may_compact where it
// switches the telling. exact says a telling by a reading rides beside it
// ([keptTellings.led]), which the launch may hand instead, so the log says so.
func (r *Runner) judged(ctx context.Context, f firstRequest, path, brief string, exact bool) bool {
	if !f.compacts(brief) {
		return false
	}
	bound, _ := f.count.Bound()
	var mark sandbox.CountMark
	var reported int64
	if f.count != nil {
		mark, reported = f.count.Mark, f.count.Window
	}
	window := int64(f.window.Standard)
	if reported > 0 && reported < window {
		window = reported
	}
	outcome := "the continuation is told as a cut-off one, and its guard arms no skill"
	if exact {
		outcome += ", unless the launch hands the brief told by its reading (executor_session_holdings_read; " +
			"where it does not, executor_session_holdings_unused says why)"
	}
	log.InfoContext(ctx, "executor_session_may_compact", "agent_id", r.cfg.Turn.AgentID,
		"turn_id", r.cfg.Turn.RunID, "path", path, "bound", bound, "mark", string(mark),
		"reported_window", reported, "standard_window", f.window.Standard, "known", f.window.Known,
		"brief_tokens", sandbox.ClaudeCharge(len(brief)), "point", sandbox.CompactionPoint(window),
		"outcome", outcome)
	return true
}

// keptTellings is the kept path's tellings its verdict may hand, built before
// the relaunch's last ask of the seat's budget, as every brief is: the held one
// ([sessionHolds]) and the one told as a session that may be a summary, each
// only where [firstRequest.settled] does not rule it out. The lead, and so the
// verdict, comes once the call a person approved is made ([keptTellings.led]).
//
// exact is told by a reading of what a cut-off session holds, where the verdict
// is the brief's ([Runner.heldReading]): as the held telling, as calls it may
// already show, minus the calls the reading finds held, with an unreported
// subagent's calls under its name. Nil where none was asked or the reading reads
// or moves no call. Its rewrites are spent before the budget's last ask, a piece
// the held telling fitted answered from the compactor's cache.
type keptTellings struct {
	f            firstRequest
	holds        sessionHolds
	held, summed *keptBrief
	exact        *keptBrief
}

// keptTellings builds the kept path's tellings of basis ([Runner.keptSession]);
// nothing where basis names no kept session.
//
// THE READING IS ASKED HERE, ONCE PER PATH, never per telling, and only of a
// cut-off capture ([SessionBasis.MayLackBrief], the only kind a reading reads)
// whose verdict is byBrief: a reading may be used only where the first request
// is judged to hand the model the history it read ([firstRequest.judges]), never
// for a session that compacts whatever its brief. A restart in the capture's job
// is no bar: the reading counts a call only past the main thread's last restart,
// and the telling it thins keeps every word a session that may be a summary is
// told.
func (r *Runner) keptTellings(ctx context.Context, basis SessionBasis, round roundCalls, current int,
	leadHeld []toldHeld, history []ledger.Iteration, surface *tools.Surface, fresh []steer.Note,
	branch string) keptTellings {

	t := keptTellings{f: firstRequest{harness: basis.Harness, window: basis.Window, count: keptCount(basis, r.cfg.Resume)},
		holds: sessionHolds{cutOff: basis.MayLackBrief(),
			restartedInJob: basis.Harness == sandbox.ClaudeCodeHarness && keptRestarted(basis, r.cfg.Resume),
			restarted:      basis.Harness == sandbox.ClaudeCodeHarness && restartedAnywhere(basis.Count)}}
	if !basis.Continues() {
		return t
	}
	verdict := t.f.settled()
	if verdict != compactsWhatever {
		t.held = r.keptSession(ctx, basis, round, current, leadHeld, history, surface, fresh, branch, t.holds, nil)
	}
	if verdict != heldWhatever {
		summarized := t.holds
		summarized.compacts = true
		t.summed = r.keptSession(ctx, basis, round, current, leadHeld, history, surface, fresh, branch, summarized,
			nil)
	}
	if verdict == byBrief && basis.MayLackBrief() {
		if read := r.heldReading(ctx, basis, round, t.holds); read != nil {
			t.exact = r.keptSession(ctx, basis, round, current, leadHeld, history, surface, fresh, branch, t.holds,
				read)
		}
	}
	return t
}

// led is the kept continuation the path's verdict picks, led by lead and own (the
// leads its launch keeps), with the value its telling and guard were decided by:
// the held telling where it sends the session whole, else the summarized one.
// Judged on the LONGER of the held telling's two briefs (the one for another box),
// as the launch's floor reads them; monotone, since the summarized telling is
// never the shorter and is handed wherever the held one may compact.
//
// The exact telling ([SessionContinuation.Exact]) is decided FIRST, for the
// default's log line; attached only where [firstRequest.judges] and its OWN brief
// sends whole (else executor_session_holdings_unused), since it drops held calls
// and may add sections. [Runner.sessionDecided] reads its value (exactHolds)
// where the launch hands it; its Resumer is [SessionBasis.Release].
func (t keptTellings) led(ctx context.Context, r *Runner, lead string,
	own []sandbox.Lead) (*SessionContinuation, sessionHolds, exactHolds) {

	var exact *ExactTelling
	var value exactHolds
	if t.exact != nil && t.f.judges() {
		told := t.exact.led(lead, own)
		if !t.f.compacts(told.Replaced) {
			exact = &ExactTelling{Resumer: t.exact.basis.Release, Brief: told.Brief, Replaced: told.Replaced,
				Told: told.Told}
			value = exactHolds{holds: t.holds, held: t.exact.readHeld}
		} else {
			log.InfoContext(ctx, "executor_session_holdings_unused", "agent_id", r.cfg.Turn.AgentID,
				"turn_id", r.cfg.Turn.RunID,
				"detail", "its harness may compact the session before it reads the brief told by the reading",
				"outcome", "the launch is offered the session told without the reading; "+
					"executor_session_not_continued says so where the session itself is not continued")
		}
	}
	picked, holds := t.held.led(lead, own), t.holds
	if t.summed != nil {
		summed := t.summed.led(lead, own)
		brief := summed.Replaced
		if picked != nil {
			brief = picked.Replaced
		}
		if r.judged(ctx, t.f, "kept", brief, exact != nil) {
			picked, holds.compacts = summed, true
		}
	}
	if picked != nil {
		picked.Exact = exact
	}
	return picked, holds, value
}

// exactHolds is the value the exact telling was decided by ([keptTellings.led]):
// the held telling's (holds) and the calls its reading finds held (held), which
// the guard counts beside its save point's ([Runner.sessionDecided]). Zero where
// no exact telling rides beside the default.
type exactHolds struct {
	holds sessionHolds
	held  []BridgedCall
}

// inPlaceTelling is one telling of the copy a job's box holds, but for its lead
// ([Runner.inPlaceAfter], [Runner.inPlaceNotes]): what follows the lead, the
// reads of the round it counts without listing them, the leads it tells again,
// and the notes.
type inPlaceTelling struct {
	after   []string
	omitted []int
	told    toldAgain
	notes   []string
}

// led is the telling's brief, led by lead.
func (t inPlaceTelling) led(lead string) string {
	brief := lead
	for _, part := range t.after {
		brief += "\n\n" + part
	}
	if len(t.notes) > 0 {
		brief += "\n\n" + strings.Join(t.notes, "\n\n")
	}
	return brief
}

// inPlaceTellings is the in-place path's tellings its verdict may hand, as
// [keptTellings] is the kept path's.
type inPlaceTellings struct {
	f            firstRequest
	holds        sessionHolds
	held, summed *inPlaceTelling
}

// inPlaceTellings builds the in-place path's tellings of the copy carry
// continues in its job's box.
func (r *Runner) inPlaceTellings(ctx context.Context, carry carried, basis SessionBasis, round roundCalls,
	current int, history []ledger.Iteration, surface *tools.Surface, fresh []steer.Note) inPlaceTellings {

	t := inPlaceTellings{holds: sessionHolds{cutOff: r.copyMayLackBrief()}}
	if r.cfg.Resume != nil {
		t.f = firstRequest{harness: r.cfg.Resume.Run.Harness, window: basis.Window, count: r.cfg.Resume.Count}
		t.holds.restartedInJob = t.f.harness == sandbox.ClaudeCodeHarness && inPlaceRestarted(r.cfg.Resume)
		t.holds.restarted = t.f.harness == sandbox.ClaudeCodeHarness && restartedAnywhere(r.cfg.Resume.Count)
	}
	build := func(holds sessionHolds) *inPlaceTelling {
		after, omitted, told := r.inPlaceAfter(ctx, carry, round, current, history, surface, holds)
		return &inPlaceTelling{after: after, omitted: omitted, told: told,
			notes: r.inPlaceNotes(carry, fresh, holds)}
	}
	verdict := t.f.settled()
	if carry.session() == "" || !carry.boxKept() || !carry.keeps() {
		// NO COPY CONTINUED IN THAT BOX: one telling, of the notes alone, and
		// nothing to judge.
		verdict = heldWhatever
	}
	if verdict != compactsWhatever {
		t.held = build(t.holds)
	}
	if verdict != heldWhatever {
		summarized := t.holds
		summarized.compacts = true
		t.summed = build(summarized)
	}
	return t
}

// led is the in-place telling the path's verdict picks, led by lead, with the
// value its telling and guard were decided by ([keptTellings.led]).
func (t inPlaceTellings) led(ctx context.Context, r *Runner, lead string) (inPlaceTelling, sessionHolds) {
	holds := t.holds
	switch {
	case t.summed == nil:
		return *t.held, holds
	case t.held != nil && !r.judged(ctx, t.f, "in_place", t.held.led(lead), false):
		return *t.held, holds
	case t.held == nil:
		r.judged(ctx, t.f, "in_place", t.summed.led(lead), false)
	}
	holds.compacts = true
	return *t.summed, holds
}
