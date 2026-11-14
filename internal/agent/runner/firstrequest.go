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
	// count is what Claude Code counts the session at, nil where nothing
	// counted it.
	count *sandbox.ResumedCount
}

// firstRequestVerdict is what a path's first request is known to do before any
// brief is built.
type firstRequestVerdict int

const (
	// byBrief: the brief decides; both tellings are built, and the verdict is
	// asked of the held one ([firstRequest.compacts]).
	byBrief firstRequestVerdict = iota
	// heldWhatever: no rule of Claude Code's compacts the session (another
	// harness's): the held telling, whatever the brief.
	heldWhatever
	// compactsWhatever: a count with no bound, an unknown window, or a count
	// alone at or past the point: the compacted telling, whatever the brief.
	compactsWhatever
)

// settled is the verdict before any brief is built: another harness's session
// is held whatever its brief, and one that does not send whole with no brief at
// all compacts whatever its brief, since a longer brief never sends whole where
// a shorter one does not.
func (f firstRequest) settled() firstRequestVerdict {
	switch {
	case f.harness != sandbox.ClaudeCodeHarness:
		return heldWhatever
	case !f.window.SendsWhole(f.count, ""):
		return compactsWhatever
	}
	return byBrief
}

// compacts is THE VERDICT: whether Claude Code may compact the session before
// it reads brief. False for another harness's session. The one call into the
// predicate in this package.
func (f firstRequest) compacts(brief string) bool {
	return f.harness == sandbox.ClaudeCodeHarness && !f.window.SendsWhole(f.count, brief)
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
	// may lack the job's brief and the calls before it. A Claude Code
	// session's alone, whose thread a count reads; false for another
	// harness's, which no count describes.
	restartedInJob bool
	// compacts says its harness may compact it before its first request
	// ([firstRequest.compacts]).
	compacts bool
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

// keptCalls is how a kept session of basis is told the calls of its round, as h
// decides, in ONE EXPRESSION: from where the round's calls are told, the reads
// its job's brief counted without listing them told first, and whether they are
// told as calls it may already show. The calls after its save point, owed reads
// first, as calls it lacks where h counts it as holding what its record says;
// the same as calls it may already show where it may lack its job's brief or
// its harness may compact it ([sessionHolds.lacks]); and every call of the
// round from its start, owing none, so, where it may be a summary
// ([sessionHolds.summarized]). A reading of what the session holds swaps this
// one expression for its own answer.
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
// window it was judged under and the point.
func (r *Runner) judged(ctx context.Context, f firstRequest, path, brief string) bool {
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
	log.InfoContext(ctx, "executor_session_may_compact", "agent_id", r.cfg.Turn.AgentID,
		"turn_id", r.cfg.Turn.RunID, "path", path, "bound", bound, "mark", string(mark),
		"reported_window", reported, "standard_window", f.window.Standard, "known", f.window.Known,
		"brief_tokens", sandbox.ClaudeCharge(len(brief)), "point", sandbox.CompactionPoint(window),
		"outcome", "the continuation is told as a cut-off one, and its guard arms no skill")
	return true
}

// keptTellings is the kept path's tellings its verdict may hand, built before
// the relaunch's last ask of the seat's budget, as every brief is: the held one,
// told as the record's half of its value says ([sessionHolds]), and the one told
// as a session that may be a summary, each only where the verdict settled
// before any brief does not rule it out ([firstRequest.settled]). Its lead, and
// so the verdict, comes once the call a person approved is made
// ([keptTellings.led]).
type keptTellings struct {
	f            firstRequest
	holds        sessionHolds
	held, summed *keptBrief
}

// keptTellings builds the kept path's tellings of basis ([Runner.keptSession]).
// Nothing where basis names no kept session.
func (r *Runner) keptTellings(ctx context.Context, basis SessionBasis, round roundCalls, current int,
	leadHeld []string, history []ledger.Iteration, surface *tools.Surface, fresh []steer.Note,
	branch string) keptTellings {

	t := keptTellings{f: firstRequest{harness: basis.Harness, window: basis.Window, count: keptCount(basis, r.cfg.Resume)},
		holds: sessionHolds{cutOff: basis.MayLackBrief(),
			restartedInJob: basis.Harness == sandbox.ClaudeCodeHarness && keptRestarted(basis, r.cfg.Resume)}}
	if !basis.Continues() {
		return t
	}
	verdict := t.f.settled()
	if verdict != compactsWhatever {
		t.held = r.keptSession(ctx, basis, round, current, leadHeld, history, surface, fresh, branch, t.holds)
	}
	if verdict != heldWhatever {
		summarized := t.holds
		summarized.compacts = true
		t.summed = r.keptSession(ctx, basis, round, current, leadHeld, history, surface, fresh, branch, summarized)
	}
	return t
}

// led is the kept continuation the path's verdict picks, led by lead, the leads
// own its launch keeps, with the value its telling and guard were decided by:
// the held telling where its harness sends the session whole with the held
// brief, and else the one told as a session that may be a summary. JUDGED ON
// THE LONGER OF THE HELD TELLING'S TWO BRIEFS, as the launch's floor reads
// them: the one for a box other than the one the session was last worked in.
// Monotone, so never an oscillation: the compacted telling is never the shorter,
// and handed wherever the held one may compact, whatever its own length.
func (t keptTellings) led(ctx context.Context, r *Runner, lead string,
	own []sandbox.Lead) (*SessionContinuation, sessionHolds) {

	held, holds := t.held.led(lead, own), t.holds
	if t.summed == nil {
		return held, holds
	}
	summed := t.summed.led(lead, own)
	brief := summed.Replaced
	if held != nil {
		brief = held.Replaced
	}
	if !r.judged(ctx, t.f, "kept", brief) {
		return held, holds
	}
	holds.compacts = true
	return summed, holds
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
	case t.held != nil && !r.judged(ctx, t.f, "in_place", t.held.led(lead)):
		return *t.held, holds
	case t.held == nil:
		r.judged(ctx, t.f, "in_place", t.summed.led(lead))
	}
	holds.compacts = true
	return *t.summed, holds
}
