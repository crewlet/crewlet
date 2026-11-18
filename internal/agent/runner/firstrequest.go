package runner

import (
	"context"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// WHETHER A CONTINUED SESSION REACHES ITS RUN'S FIRST REQUEST WHOLE.
//
// A relaunch that continues a session tells it what it lacks and counts the
// rest as held: the copy a job's box holds whose harness ended its own run holds
// every call of the round but the late ones, every note it read and its brief;
// a kept capture holds the calls at or below its save point. Claude Code may not
// hand its model that session. It compacts a session before a request, the
// resumed run's first among them, where its own count of the conversation with
// the brief and what the resume attaches reaches its threshold, and the model
// then reads a summary of everything before its last reply
// (internal/sandbox's SendsWhole); and a session its job restarted after its
// brief (a compaction as it worked) is handed nothing before that restart but
// what the restart left. Whatever the engine counted as held can then be gone.
//
// So each path a relaunch may continue decides ONE VALUE, once, at the one place
// its telling is decided ([sessionHolds]): whether the record says the session
// may lack its job's brief (cut off, or never said to have ended its own run),
// whether its job restarted it after its brief, and whether its harness may
// compact it before it reads the brief this launch hands it ([firstRequest]),
// which is asked of the brief the held telling hands, so after the briefs and
// the lead exist. Every telling of the path and its guard read that one value:
// a continuation that may lack what it is counted as holding is told as a
// cut-off one (every call of its round, every note the turn has read, the leads
// its brief told, all of them where it may be a summary), and its guard arms no
// skill a summary may hold only in words.
//
// AND WHETHER ITS THREAD RESTARTED AT ALL, BEFORE ITS JOB OR IN IT
// ([sessionHolds.restarted]): a session that restarted in an earlier job holds
// what came before that restart only in the restart's summary, and the telling
// that followed it told that job's round again from its start, never the
// turn's closed rounds before it. So such a session may hold the closed rounds
// only in a summary's words ([sessionHolds.closedSummarized]), which no value
// of a listed call can be compared with: its brief says so of the tools its
// told calls name ([Runner.otherCalls]).
//
// AND A KEPT SESSION CUT OFF IS READ FOR THE CALLS IT HOLDS, where this build
// judges its first request ([firstRequest.judges]) by its brief
// ([keptTellings]): the reading, asked once per path, before any brief is
// led, tells a third telling of the path, told only the calls the session
// does not hold, which the launch may hand in place of the default
// ([ExactTelling]). ONE VALUE PER TELLING THE LAUNCH MAY HAND: the default's,
// decided once by its verdict as above, and the third telling's, the held
// telling's value, judged on that telling's own brief ([keptTellings.led]),
// since a brief that drops held calls and adds sections of its own is not
// ordered against the held one. The guard reads the value of the telling the
// launch handed ([Runner.sessionDecided]).

// firstRequest is how a relaunch judges whether the session a path continues
// holds, at its run's first request, what a telling that counts it as held says
// it holds: Claude Code compacts a session before that request where its count
// of the conversation, the brief and what the turn attaches reaches its
// threshold (internal/sandbox's SendsWhole), which leaves the model a summary.
type firstRequest struct {
	// harness is the harness that wrote the session, the only one that
	// continues it.
	harness string
	// window is what the session's harness may hold the run to: the least
	// standard window of the chain's Claude Code members
	// ([SessionBasis.Window]).
	window sandbox.ExecutorWindow
	// count is what the session's harness counts it at, nil where nothing
	// counted it, and provider the provider of the reply an OpenCode count is
	// anchored on (sandbox.SessionCapture.CountProvider), "" for any other.
	count    *sandbox.ResumedCount
	provider string
}

// firstRequestVerdict is what a path's first request is known to do before any
// brief is built.
type firstRequestVerdict int

const (
	// byBrief: the brief decides; both tellings are built, and the verdict is
	// asked of the held one ([firstRequest.compacts]); and, of a cut-off
	// capture a reading thins, a third, told by that reading, judged on its
	// own brief ([keptTellings.led]).
	byBrief firstRequestVerdict = iota
	// heldWhatever: no rule of Claude Code's compacts the session (another
	// harness's): the held telling, whatever the brief.
	heldWhatever
	// compactsWhatever: a count with no bound, an unknown window, or a count
	// alone at or past the point: the compacted telling, whatever the brief.
	compactsWhatever
	// byBox: the brief and the box decide ([firstRequest.byBox]): both
	// tellings are built and handed, and the launch hands the held one only
	// where the box's point proves it whole (sandbox.OpenCodePoint.SendsWhole).
	byBox
)

// judges reports whether this build judges the path's first request: Claude
// Code's here, by its window (internal/sandbox's SendsWhole), and OpenCode's
// in the box the launch gets ([firstRequest.byBox]). THE ONE PREDICATE the
// verdicts and the attach of a telling by a reading read: a session of a
// harness this build does not judge is held whatever its brief, and is never
// handed a brief told by a reading (ADR-0073, a reading's caller judges the
// continuation's first request).
func (f firstRequest) judges() bool {
	return f.harness == sandbox.ClaudeCodeHarness || f.harness == sandbox.OpenCodeHarness
}

// bounded reports whether the path's count has a bound
// (sandbox.ResumedCount.Bound).
func (f firstRequest) bounded() bool {
	_, ok := f.count.Bound()
	return ok
}

// byBox reports whether the path's first request is the launch's to judge, in
// the box it gets: OpenCode compacts at a point its box's catalog and
// documents set for the run's model, which only that box can say.
func (f firstRequest) byBox() bool { return f.harness == sandbox.OpenCodeHarness }

// settled is the verdict before any brief is built: the session of a harness
// this build does not judge is held whatever its brief, and one that does not
// send whole with no brief at all compacts whatever its brief, since a longer
// brief never sends whole where a shorter one does not.
func (f firstRequest) settled() firstRequestVerdict {
	switch {
	case !f.judges():
		return heldWhatever
	case f.byBox() && !f.bounded():
		// NO BOUND, NO POINT CAN PROVE IT WHOLE: the launch is handed the
		// summarized telling alone, and reads nothing.
		return compactsWhatever
	case f.byBox():
		return byBox
	case !f.window.SendsWhole(f.count, ""):
		return compactsWhatever
	}
	return byBrief
}

// compacts is THE VERDICT: whether the session's harness may compact it
// before it reads brief. False for the session of a harness this build does
// not judge ([firstRequest.judges]); for one the box judges ([firstRequest.byBox]),
// whether its count has no bound, whatever the brief: the runner never judges
// a brief against a point it cannot read. The one call into the predicate in
// this package.
func (f firstRequest) compacts(brief string) bool {
	if f.byBox() {
		return !f.bounded()
	}
	return f.judges() && !f.window.SendsWhole(f.count, brief)
}

// sessionHolds is what a path's telling may count the session it continues as
// holding at its run's first request: decided once per path, after the path's
// tellings are built and the lead exists, and read by every telling of that
// path and by its guard.
type sessionHolds struct {
	// cutOff says the record holds the session may lack its job's brief: a
	// capture its harness was cut off in ([SessionBasis.MayLackBrief]), the
	// copy in a box whose job's harness is not known to have ended its own
	// run ([Runner.copyMayLackBrief]). Its own words stand where it is so.
	cutOff bool
	// restartedInJob says the job's main thread restarted after its brief, or
	// whether it did cannot be read (sandbox.RestartedSince): a resume hands
	// its model nothing before the restart but what it left, so the session
	// may lack the job's brief and the calls before it. A session of a
	// harness this build judges alone ([firstRequest.judges]), whose count
	// names its restarts; false for another harness's.
	restartedInJob bool
	// compacts says its harness may compact it before its first request
	// ([firstRequest.compacts]).
	compacts bool
	// restarted says the session's thread restarted somewhere, before its job
	// or in it, or whether it did cannot be read ([restartedAnywhere]): what
	// it holds from before its newest restart is that restart's summary, or
	// nothing. A session of a harness this build judges alone
	// ([firstRequest.judges]), whose count names its restarts; false for
	// another harness's. It decides only how the turn's closed rounds are
	// told ([sessionHolds.closedSummarized]), never the round's own calls: a
	// restart in an earlier job of the round
	// was followed by a telling of the round from its start, and one in a
	// closed round is before every call of this one.
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
// rounds only in a summary's words: it may be a summary now (summarized), or
// its thread restarted at some point. The telling that followed a restart in
// an earlier job told that job's round again from its start ([sessionHolds.keptCalls]),
// never the closed rounds before it, which survive only in the restart's
// summary. A restart in an earlier turn's session counts too: an over-telling,
// the safe side.
func (h sessionHolds) closedSummarized() bool { return h.summarized() || h.restarted }

// restartedAnywhere reports whether count finds the session's thread restarted
// at some point, or cannot read whether it did (sandbox.ResumedCount.Restarted).
func restartedAnywhere(count *sandbox.ResumedCount) bool {
	newest, read := count.Restarted()
	return !read || newest != ""
}

// keptCalls is how a kept session of basis is told the calls of its round, as h
// decides, in ONE EXPRESSION: from where the round's calls are told, the reads
// its job's brief counted without listing them told first, and whether they are
// told as calls it may already show. The calls after its save point, owed reads
// first, as calls it lacks where h counts it as holding what its record says;
// the same as calls it may already show where it may lack its job's brief or
// its harness may compact it ([sessionHolds.lacks]); and every call of the
// round from its start, owing none, so, where it may be a summary
// ([sessionHolds.summarized]). A reading of what a cut-off session holds is
// asked of the calls this expression tells and handed to the telling beside it
// ([Runner.heldReading], [Runner.calledAfter]), which tells none it holds.
func (h sessionHolds) keptCalls(basis SessionBasis) (from *int, owed []int, mayShow bool) {
	if h.summarized() {
		return new(0), nil, true
	}
	return &basis.Through, basis.Owed, h.lacks()
}

// keptCount is the count a kept path's verdict is asked of: the capture's own
// where its harness was cut off, since the archive is then always restored over
// the box's copy; else the worse of the capture's and the count of the copy its
// job's box holds (resume's, the job before's own record), since a box that
// took the capture whole resumes its own unscrubbed copy, which a redaction may
// have made longer than the archive.
func keptCount(basis SessionBasis, resume *Resume) *sandbox.ResumedCount {
	if basis.Interrupted || resume == nil {
		return basis.Count
	}
	return basis.Count.Worse(resume.Count)
}

// keptCountProvider is the provider of the reply keptCount's count is
// anchored on: the capture's where its count is the one taken, else the
// capture's where the copy's own record names the same, and "" where the two
// differ, which bounds nothing (sandbox.OpenCodePoint.SendsWhole).
func keptCountProvider(basis SessionBasis, resume *Resume) string {
	if basis.Interrupted || resume == nil || basis.CountProvider == resume.CountProvider {
		return basis.CountProvider
	}
	return ""
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
// path was left to its brief, and logged where it switches the telling:
// executor_session_may_compact names the path, the bound or the mark, the
// window it was judged under and the point. exact says a telling by a reading
// rides beside the telling it switches to ([keptTellings.led]), which the
// launch may hand in its place, so the line's outcome says so.
func (r *Runner) judged(ctx context.Context, f firstRequest, path, brief string, exact bool) bool {
	if !f.compacts(brief) {
		return false
	}
	bound, _ := f.count.Bound()
	if f.byBox() {
		// NO BOX WAS READ: the count has no bound, which no point proves whole.
		var mark sandbox.CountMark
		if f.count != nil {
			mark = f.count.Mark
		}
		log.InfoContext(ctx, "executor_session_may_compact", "agent_id", r.cfg.Turn.AgentID,
			"turn_id", r.cfg.Turn.RunID, "path", path, "bound", bound, "mark", string(mark),
			"provider", f.provider, "brief_tokens", sandbox.OpenCodeCharge(int64(len(brief))),
			"outcome", "the count has no bound, so no box's point can prove the session whole: the "+
				"continuation is told as a cut-off one, and its guard arms no skill")
		return true
	}
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
// the relaunch's last ask of the seat's budget, as every brief is: the held one,
// told as the record's half of its value says ([sessionHolds]), and the one told
// as a session that may be a summary, each only where the verdict settled
// before any brief does not rule it out ([firstRequest.settled]). Its lead, and
// so the verdict, comes once the call a person approved is made
// ([keptTellings.led]).
//
// AND THE ONE TOLD BY A READING OF WHAT THE SESSION HOLDS (exact), where the
// session was cut off and the verdict is the brief's to decide
// ([Runner.heldReading]): told by the held telling's value, so as calls the
// session may already show, but told none of the calls the reading finds it
// holding, and the calls the reading finds in an unreported subagent's thread
// under that subagent's name. Nil where none was asked, the reading read none,
// or it holds and moves no call, where it would tell what the held telling
// tells. Its rewrites are spent before the budget's last ask with the other
// two's, a piece the held telling fitted answered from the compactor's cache.
type keptTellings struct {
	f            firstRequest
	holds        sessionHolds
	held, summed *keptBrief
	exact        *keptBrief
}

// keptTellings builds the kept path's tellings of basis ([Runner.keptSession]),
// nothing where basis names no kept session. THE READING IS ASKED HERE, once
// per path ([Runner.heldReading]): only of a capture whose harness was cut off
// ([SessionBasis.MayLackBrief]), and only where the verdict before any brief
// is the brief's or the box's (byBrief, byBox), since a reading may be used
// only where its caller judges that the first request hands its model the
// history the reading read. A restart in the capture's job is no bar: the
// reading counts a call only past the thread's last restart, and the telling
// it thins keeps every word a session that may be a summary is told.
func (r *Runner) keptTellings(ctx context.Context, basis SessionBasis, round roundCalls, current int,
	leadHeld []toldHeld, history []ledger.Iteration, surface *tools.Surface, fresh []steer.Note,
	branch string) keptTellings {

	t := keptTellings{f: firstRequest{harness: basis.Harness, window: basis.Window, count: keptCount(basis, r.cfg.Resume),
		provider: keptCountProvider(basis, r.cfg.Resume)}, holds: sessionHolds{cutOff: basis.MayLackBrief()}}
	t.holds.restartedInJob = t.f.judges() && keptRestarted(basis, r.cfg.Resume)
	t.holds.restarted = t.f.judges() && restartedAnywhere(basis.Count)
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
	if (verdict == byBrief || verdict == byBox) && basis.MayLackBrief() {
		if read := r.heldReading(ctx, basis, round, t.holds); read != nil {
			t.exact = r.keptSession(ctx, basis, round, current, leadHeld, history, surface, fresh, branch, t.holds,
				read)
		}
	}
	return t
}

// led is the kept continuation the path's verdict picks, led by lead, the leads
// own its launch keeps, with the value its telling and guard were decided by:
// the held telling where it sends the session whole, judged on the longer of
// its two briefs as the floor reads them, else the summarized one (monotone:
// the summarized telling is never the shorter); under byBox both, the held
// one picked. The telling by a reading ([SessionContinuation.Exact]) is
// decided first, on its own brief (the package doc says why), and attached
// where it sends whole or the box judges it; else executor_session_holdings_unused
// says why. One value per telling the launch may hand: the default's, and the
// reading's (exactHolds), which the guard reads where the launch hands it
// ([Runner.sessionDecided]). Its Resumer is the session's writer
// ([SessionBasis.Release]), the one release whose box is handed its briefs.
func (t keptTellings) led(ctx context.Context, r *Runner, lead string,
	own []sandbox.Lead) (*SessionContinuation, sessionHolds, exactHolds) {

	var exact *ExactTelling
	var value exactHolds
	if t.exact != nil && t.f.judges() {
		told := t.exact.led(lead, own)
		if t.f.byBox() || !t.f.compacts(told.Replaced) {
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
	if t.summed != nil && t.held != nil && t.f.byBox() {
		// BOTH TO THE LAUNCH, which judges them in its box: the held telling
		// picked, the summarized one beside it, with the count it is judged by.
		summed := t.summed.led(lead, own)
		picked.Summarized = &SummarizedTelling{Brief: summed.Brief, Replaced: summed.Replaced, Told: summed.Told}
		picked.Count, picked.CountProvider = t.f.count, t.f.provider
	} else if t.summed != nil {
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

// exactHolds is the value a kept path's telling by a reading was decided by
// ([keptTellings.led]): the held telling's (holds), its own brief judged to
// send the session whole, and the calls its reading finds the session holding
// (held), which its guard counts beside the ones its save point holds
// ([Runner.sessionDecided]). Zero where no telling by a reading rides beside
// the default.
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
		t.f = firstRequest{harness: r.cfg.Resume.Run.Harness, window: basis.Window, count: r.cfg.Resume.Count,
			provider: r.cfg.Resume.CountProvider}
		t.holds.restartedInJob = t.f.judges() && inPlaceRestarted(r.cfg.Resume)
		t.holds.restarted = t.f.judges() && restartedAnywhere(r.cfg.Resume.Count)
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
// value its telling and guard were decided by ([keptTellings.led]); and,
// where the launch judges the first request in the box ([firstRequest.byBox]),
// the summarized telling it hands where the box's point does not prove the
// held one whole, nil elsewhere.
func (t inPlaceTellings) led(ctx context.Context, r *Runner, lead string) (inPlaceTelling, sessionHolds,
	*inPlaceTelling) {

	holds := t.holds
	switch {
	case t.summed == nil:
		return *t.held, holds, nil
	case t.held != nil && t.f.byBox():
		return *t.held, holds, t.summed
	case t.held != nil && !r.judged(ctx, t.f, "in_place", t.held.led(lead), false):
		return *t.held, holds, nil
	case t.held == nil:
		r.judged(ctx, t.f, "in_place", t.summed.led(lead), false)
	}
	holds.compacts = true
	return *t.summed, holds, nil
}
