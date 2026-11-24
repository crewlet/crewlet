package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// WHETHER A CONTINUED SESSION REACHES ITS FIRST REQUEST WHOLE. Claude Code compacts
// before a request where its count of the session, with the brief and what the
// resume attaches, reaches 167,000 tokens under a 200,000-token window; a
// continuation that may compact there, or whose job restarted the session after
// its brief, is told as a cut-off one.

// standard200k is the window a claude-sonnet-4-5 member holds a run to.
var standard200k, _ = executor.Window(executor.ClaudeCodeName,
	executor.WindowAsk{Model: "claude-sonnet-4-5", SignIn: executor.SignInFirstParty})

// countOf is a count of tokens read whole, its thread restarting nowhere.
func countOf(tokens int64) *sandbox.ResumedCount {
	return &sandbox.ResumedCount{Tokens: tokens, Restart: new("")}
}

// near and far are counts a 200,000-token window compacts at the first request
// whatever the brief, and sends whole with any brief a case hands.
var near, far = countOf(160_000), countOf(10_000)

// inBoxBasis is a probe that continues no kept session, under a known window.
var inBoxBasis = runner.SessionBasis{
	Window: standard200k}

// keptClaude is the turn's kept session of a Claude Code job, saved at the
// round's first call, counted as count.
func keptClaude(count *sandbox.ResumedCount, interrupted bool) runner.SessionBasis {
	return runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1", Harness: "claude-code",
		Through: 1, NoteCursor: 1, Home: "/boxes/4f2a/home", Interrupted: interrupted, Window: standard200k,
		Count: count}
}

// copyOf is what a job's record says of the copy its box holds: how its harness
// ended, Claude Code's count of it, and as the restart its launch handed it,
// the newest restart that count read.
func copyOf(end sandbox.HarnessEnd, count *sandbox.ResumedCount) func(*runner.Resume) {
	return func(r *runner.Resume) {
		r.HarnessEnd, r.Count, r.Run.Harness = end, count, "claude-code"
		if newest, read := count.Restarted(); read {
			r.BasisRestart = &newest
		}
	}
}

// answered carries the round on with a person's answer, in the job's own box.
func answered(words string) func(*runner.Resume) {
	return func(r *runner.Resume) {
		r.Asked = &runner.Asked{Question: "which branch?", Answer: words, By: "founder", SessionID: "sess-1",
			BoxKept: true}
	}
}

// inPlaceOf is the brief g's relaunch hands the copy in the job's box.
func inPlaceOf(t *testing.T, g guarded) string {
	t.Helper()
	req := g.relaunch(t)
	if req.Continue == nil || req.Continue.SessionID != "sess-1" {
		t.Fatalf("the relaunch continues %+v in place, want the job's own copy", req.Continue)
	}
	return req.Continue.Brief
}

// earlierCall is the round's first call, made before the session's save point.
var earlierCall = numbered("jira_create", map[string]any{"summary": "an earlier issue"}, 1, 1)

// laterCall is the round's second call, made after it.
var laterCall = numbered("jira_create", map[string]any{"summary": "a later issue"}, 2, 2)

// readNote is a note the turn's job read at its first take, numbered 1: at or
// below every kept capture's note cursor here, so a session held whole is never
// told it.
var readNote = steer.Read{Note: steer.Note{ID: "read-1", Text: "keep the release notes short", By: "founder"},
	Reading: steer.Reading{Phase: types.PhaseExecute, Iteration: 1, Round: 1,
		Boundary: types.SteerBoundaryToolBatch, Launch: "job-1", Order: 1}}

// keptAnswer is an answer an earlier job of the turn opened with, kept beside
// the run's record and untold by its job's own brief.
var keptAnswer = keptLead(answerOf("job-0", 1, 1, "use the release branch"))

// toldPath is what one path's continuation was handed: its brief, and whether
// the guard its launch armed refused a skill-covered call for its skill.
type toldPath struct {
	brief   string
	refused bool
}

// continuedDown relaunches g down one path (the kept capture where kept, else
// the copy in its job's box) and plays a skill-covered call on the guard the
// launch armed.
func continuedDown(t *testing.T, g guarded, kept bool) toldPath {
	t.Helper()
	req := g.relaunch(t)
	var told toldPath
	switch {
	case kept && req.Session != nil:
		told.brief = req.Session.Replaced
	case !kept && req.Continue != nil && req.Continue.SessionID == "sess-1":
		told.brief = req.Continue.Brief
	default:
		t.Fatalf("the relaunch continues %+v kept and %+v in place, want the path a case asked of it",
			req.Session, req.Continue)
	}
	bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})
	told.refused = bridged[0].Failed && strings.Contains(bridged[0].Result, "chat-conventions")
	return told
}

// toldAsASummary fails t unless told is a continuation told as one that may be
// a summary: why, then every lead its record keeps, every call of the round from
// its start and every note the turn has read, in that order, as messages it may
// already show; and a guard that arms no skill. Its record says its harness
// ended its own run, so no line says it was, or may have been, cut off, the
// leads' section included; save under [conditionalOnTheBase], which speaks of
// where the session began, never of this run.
func toldAsASummary(t *testing.T, name string, told toldPath) {
	t.Helper()
	section, leads := strings.Index(told.brief, prompts.MayCompactSession), strings.Index(told.brief, leadsHeading)
	calls, notes := strings.Index(told.brief, cutOffHeader), strings.Index(told.brief, notesAgainHeader)
	if section < 0 || leads < section || calls < leads || notes < calls {
		t.Errorf("%s: the session is not told why, then its leads, its calls and its notes, as one that may be a "+
			"summary:\n%s", name, told.brief)
		return
	}
	if !strings.Contains(told.brief[leads:calls], "use the release branch") ||
		!strings.Contains(told.brief[calls:notes], "an earlier issue") ||
		!strings.Contains(told.brief[notes:], prompts.SteerMessage(readNote.By, readNote.Text)) {
		t.Errorf("%s: the session is not told every kept lead, every call of the round from its start and every "+
			"note the turn has read:\n%s", name, told.brief)
	}
	for _, line := range outsideTheBase(told.brief) {
		if strings.Contains(strings.ToLower(line), "cut off") {
			t.Errorf("%s: a session its record has end its own run is told %q:\n%s", name, line, told.brief)
		}
	}
	if !told.refused {
		t.Errorf("%s: the guard of a session that may be a summary arms a skill", name)
	}
}

// conditionalOnTheBase opens what a turn's task told again says of the session
// the conversation's last turn left, true only of a session that began as it
// ([prompts.NextTurnAgain]); its bullets follow, to the next non-bullet line.
const conditionalOnTheBase = "Where the session above began as the one your earlier "

// outsideTheBase is brief's lines but [conditionalOnTheBase]'s and its bullets.
func outsideTheBase(brief string) []string {
	var lines []string
	under := false
	for _, line := range strings.Split(brief, "\n") {
		switch {
		case strings.HasPrefix(line, conditionalOnTheBase):
			under = true
		case under && strings.HasPrefix(line, "- "):
		default:
			under = false
			lines = append(lines, line)
		}
	}
	return lines
}

// toldAsHeld fails t unless told is a continuation told as holding what its
// record says: none of what [toldAsASummary] asks, and a guard keeping the
// skills its launch armed.
func toldAsHeld(t *testing.T, name string, told toldPath) {
	t.Helper()
	for _, not := range []string{prompts.MayCompactSession, leadsHeading, "use the release branch", cutOffHeader,
		"an earlier issue", notesAgainHeader, prompts.SteerMessage(readNote.By, readNote.Text)} {
		if strings.Contains(told.brief, not) {
			t.Errorf("%s: a session that holds what its record says is told %q:\n%s", name, not, told.brief)
		}
	}
	if told.refused {
		t.Errorf("%s: the guard of a session that holds what its record says arms none of its launch's skills",
			name)
	}
}

// pathOf is a case of [TestEveryTellingOfAPathReadsItsOneValue] and
// [TestASessionItsJobRestartedIsToldAsOneThatMayBeASummary]: the session the
// probe answers, whether it reads the kept path, the record's count of the copy
// in its box and the restart its launch handed it, and whether the session may
// be a summary.
type pathOf struct {
	basis   runner.SessionBasis
	kept    bool
	copied  *sandbox.ResumedCount
	basisAt string
	summary bool
}

// told is c's continuation, by a person's answer in the job's own box, of a job
// whose harness ended its own run, whose round made earlierCall, which read
// readNote and whose run keeps keptAnswer; its launch armed the seat's one skill.
func (c pathOf) told(t *testing.T, active []string) toldPath {
	t.Helper()
	return continuedDown(t, guarded{basis: c.basis, continued: true, bridged: []runner.BridgedCall{earlierCall},
		state: execstate.State{ActiveTools: active, LoadedSkills: []string{"chat-conventions"}},
		carry: func(r *runner.Resume) {
			answered("use main")(r)
			copyOf(sandbox.HarnessEndOwnRun, c.copied)(r)
			r.BasisRestart = new(c.basisAt)
			r.Steered, r.Leads = []steer.Read{readNote}, []runner.ResumeLead{keptAnswer}
		}}, c.kept)
}

// AN OWN RUN'S COPY THAT MAY COMPACT IS TOLD AS A CUT-OFF ONE: it holds every
// call of the round, but its harness may hand its model a summary of them at the
// first request, so it is told why (never that it was cut off) and then every
// call from the round's start, as calls it may already show.
//
// Mutation: ignore the verdict, and this goes red.
func TestAnOwnRunCopyThatMayCompactIsToldAsACutOffOne(t *testing.T) {
	t.Parallel()
	brief := inPlaceOf(t, guarded{basis: inBoxBasis, bridged: []runner.BridgedCall{earlierCall},
		carry: func(r *runner.Resume) { answered("use main")(r); copyOf(sandbox.HarnessEndOwnRun, near)(r) }})
	section, calls := strings.Index(brief, prompts.MayCompactSession), strings.Index(brief, cutOffHeader)
	if section < 0 || calls < section || !strings.Contains(brief[calls:], "an earlier issue") {
		t.Errorf("the copy is not told why and then every call of the round:\n%s", brief)
	}
	for _, cutOff := range []string{prompts.InterruptedCopy, prompts.MayBeInterruptedCopy} {
		if strings.Contains(brief, cutOff) {
			t.Errorf("the copy whose harness ended its own run is told it was cut off:\n%s", brief)
		}
	}
}

// AN OWN RUN'S COPY THAT SENDS WHOLE IS TOLD AS HELD: its first request carries the
// session whole, so it is told none of its calls and nothing of a summary.
//
// Mutation: compact every copy, and this goes red.
func TestAnOwnRunCopyThatSendsWholeIsToldAsHeld(t *testing.T) {
	t.Parallel()
	brief := inPlaceOf(t, guarded{basis: inBoxBasis, bridged: []runner.BridgedCall{earlierCall},
		carry: func(r *runner.Resume) { answered("use main")(r); copyOf(sandbox.HarnessEndOwnRun, far)(r) }})
	if strings.Contains(brief, prompts.MayCompactSession) || strings.Contains(brief, "an earlier issue") {
		t.Errorf("a copy that sends whole is told what it holds:\n%s", brief)
	}
}

// A KEPT SESSION THAT MAY COMPACT IS TOLD EVERY CALL FROM THE ROUND'S START: a
// summary holds the calls at or below its save point in its own words alone, so
// the call before the save is told beside the one after it, both as calls it
// may already show, though its record says its harness ended its own run. This
// pins the kept path's one calls site where the session may be a summary;
// TestAKeptCaptureCutOffSaysWhetherItsJobRestarted pins it cut off, and
// TestASessionItsJobRestartedIsToldAsOneThatMayBeASummary held.
//
// Mutation: tell the calls from the save point, or under the words of calls the
// session lacks, and this goes red.
func TestAKeptSessionThatMayCompactIsToldEveryCallFromTheRoundsStart(t *testing.T) {
	t.Parallel()
	req := guarded{basis: keptClaude(near, false), bridged: []runner.BridgedCall{earlierCall, laterCall},
		carry: func(r *runner.Resume) { answered("use main")(r); copyOf(sandbox.HarnessEndOwnRun, near)(r) },
	}.relaunch(t)
	if req.Session == nil {
		t.Fatal("the relaunch continues no kept session")
	}
	brief := req.Session.Replaced
	calls := strings.Index(brief, cutOffHeader)
	if !strings.Contains(brief, prompts.MayCompactSession) || calls < 0 ||
		!strings.Contains(brief[calls:], "an earlier issue") || !strings.Contains(brief[calls:], "a later issue") {
		t.Errorf("the kept session is not told every call of the round from its start, as calls it may "+
			"already show:\n%s", brief)
	}
}

// THE KEPT PATH IS JUDGED BY THE WORSE OF ITS CAPTURE AND ITS JOB'S COPY. A capture
// taken whole continues from its box's unscrubbed copy, which a redaction may have
// made longer, so the worse of the two counts decides; a capture cut off is always
// restored over the box's copy, so its own count does. Each case reads what the
// verdict moves: one that may compact is told the call at or below its save point
// and arms no skill; one that sends whole is told only the calls after it and
// keeps the skills its launch armed, whatever its record's words.
//
// Mutation: read the capture's count alone, and the first case goes red; read a
// cut-off capture by the worse of the two, and the second does.
func TestTheKeptPathIsJudgedByTheWorseOfItsCaptureAndItsJobsCopy(t *testing.T) {
	t.Parallel()
	active := guarded{basis: inBoxBasis}.relaunch(t).Surface.Active()
	for name, tc := range map[string]struct {
		interrupted, compacts bool
	}{
		"taken whole": {compacts: true},
		"cut off":     {interrupted: true},
	} {
		told := continuedDown(t, guarded{basis: keptClaude(far, tc.interrupted), continued: true,
			bridged: []runner.BridgedCall{earlierCall},
			state:   execstate.State{ActiveTools: active, LoadedSkills: []string{"chat-conventions"}},
			carry:   func(r *runner.Resume) { answered("use main")(r); copyOf(sandbox.HarnessEndOwnRun, near)(r) },
		}, true)
		if got := strings.Contains(told.brief, prompts.MayCompactSession); got != tc.compacts {
			t.Errorf("%s: told it may compact %v, want %v:\n%s", name, got, tc.compacts, told.brief)
		}
		if got := strings.Contains(told.brief, "an earlier issue"); got != tc.compacts {
			t.Errorf("%s: told the call at or below its save point %v, want %v:\n%s", name, got, tc.compacts,
				told.brief)
		}
		if told.refused != tc.compacts {
			t.Errorf("%s: a skill-covered call refused for its skill %v, want %v", name, told.refused, tc.compacts)
		}
	}
}

// THE VERDICT IS ASKED OF THE BRIEF THE HELD TELLING HANDS, its lead among it: a
// count that sends whole with no brief at all compacts once the lead is beside it,
// so the copy is told as one that may compact.
//
// Mutation: judge before the lead, and this goes red.
func TestTheVerdictIsAskedOfTheBriefTheHeldTellingHands(t *testing.T) {
	t.Parallel()
	long := "use main, " + strings.Repeat("and keep the notes short ", 1600)
	brief := inPlaceOf(t, guarded{basis: inBoxBasis, bridged: []runner.BridgedCall{earlierCall},
		carry: func(r *runner.Resume) { answered(long)(r); copyOf(sandbox.HarnessEndOwnRun, countOf(140_000))(r) }})
	if !strings.Contains(brief, prompts.MayCompactSession) {
		t.Errorf("a lead that tips the count is not judged:\n%.400s", brief)
	}
}

// A MARK, NO COUNT OR AN UNKNOWN WINDOW IS DECIDED BEFORE ANY BRIEF: none bounds
// the count, so the copy is told as one that may compact whatever its brief.
//
// Mutation: hand the held telling where the count has no bound, and a case goes
// red.
func TestAMarkNoCountOrUnknownWindowIsDecidedBeforeAnyBrief(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		basis runner.SessionBasis
		count *sandbox.ResumedCount
	}{
		"a mark":            {basis: inBoxBasis, count: &sandbox.ResumedCount{Mark: sandbox.CountMedia, Restart: new("")}},
		"no count":          {basis: inBoxBasis},
		"an unknown window": {basis: runner.SessionBasis{}, count: far},
	} {
		brief := inPlaceOf(t, guarded{basis: tc.basis, bridged: []runner.BridgedCall{earlierCall},
			carry: func(r *runner.Resume) {
				answered("use main")(r)
				copyOf(sandbox.HarnessEndOwnRun, tc.count)(r)
				r.BasisRestart = new("")
			}})
		if !strings.Contains(brief, prompts.MayCompactSession) {
			t.Errorf("%s: the copy is not told it may compact:\n%s", name, brief)
		}
	}
}

// A SESSION THAT MAY COMPACT ARMS NO SKILL: a summary holds a skill's body in its
// own words alone, so its guard is empty and a skill-covered call is refused until
// the skill is loaded again, on the kept path and in place alike.
//
// Mutation: arm the session's loads, and a case goes red.
func TestASessionThatMayCompactArmsNoSkill(t *testing.T) {
	t.Parallel()
	active := guarded{basis: inBoxBasis}.relaunch(t).Surface.Active()
	for name, g := range map[string]guarded{
		"in place": {basis: inBoxBasis, continued: true,
			state: execstate.State{ActiveTools: active, LoadedSkills: []string{"chat-conventions"}},
			carry: func(r *runner.Resume) { answered("use main")(r); copyOf(sandbox.HarnessEndOwnRun, near)(r) }},
		"kept": {basis: keptClaude(near, false), continued: true,
			state: execstate.State{ActiveTools: active, LoadedSkills: []string{"chat-conventions"}},
			carry: func(r *runner.Resume) { answered("use main")(r); copyOf(sandbox.HarnessEndOwnRun, near)(r) }},
	} {
		req := g.relaunch(t)
		bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})
		if !bridged[0].Failed || !strings.Contains(bridged[0].Result, "chat-conventions") {
			t.Errorf("%s: a skill-covered call was not refused for its skill on a session that may be a "+
				"summary: %q", name, bridged[0].Result)
		}
	}
}

// A CUT-OFF COPY FAR FROM ITS THRESHOLD KEEPS ITS LAUNCH-ARMED SKILLS: its first
// request sends it whole, so its guard holds the skills its launch armed and none
// of its job's own loads, which it may never have written.
//
// Mutation: read a cut-off copy as compacting whatever its count, and the armed
// skill is refused.
func TestACutOffCopyFarFromItsThresholdKeepsItsLaunchArmedSkills(t *testing.T) {
	t.Parallel()
	active := guarded{basis: inBoxBasis}.relaunch(t).Surface.Active()
	req := guarded{basis: inBoxBasis, continued: true,
		state: execstate.State{ActiveTools: active, LoadedSkills: []string{"chat-conventions"}},
		carry: func(r *runner.Resume) { answered("use main")(r); copyOf(sandbox.HarnessEndCutOff, far)(r) },
	}.relaunch(t)
	bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})
	if bridged[0].Failed {
		t.Errorf("the skill its launch armed was refused: %q", bridged[0].Result)
	}
	req = guarded{basis: inBoxBasis, state: execstate.State{ActiveTools: active},
		bridged: []runner.BridgedCall{loadSkill(1)},
		carry:   func(r *runner.Resume) { answered("use main")(r); copyOf(sandbox.HarnessEndCutOff, far)(r) },
	}.relaunch(t)
	bridged = play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})
	if !bridged[0].Failed || !strings.Contains(bridged[0].Result, "chat-conventions") {
		t.Errorf("a load the cut-off job made was armed: %q", bridged[0].Result)
	}
}

// THE TOLD RECORD FOLLOWS THE TELLING HANDED: a copy that may compact is told every
// lead its record keeps, told before or not, since a compaction can drop them all,
// and the launch records them as told by this brief; a copy that sends whole is
// told none, and records none.
//
// Mutation: record the held telling's leads, or tell the told leads alone, and a
// case goes red.
func TestTheToldRecordFollowsTheTellingHanded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		count *sandbox.ResumedCount
		told  bool
	}{
		"one that may compact": {count: near, told: true},
		"one that sends whole": {count: far},
	} {
		req := guarded{basis: inBoxBasis, carry: func(r *runner.Resume) {
			answered("use main")(r)
			copyOf(sandbox.HarnessEndOwnRun, tc.count)(r)
			r.Leads = []runner.ResumeLead{keptLead(answerOf("job-0", 1, 1, "use the release branch"))}
		}}.relaunch(t)
		told := len(req.Continue.Told.Told) == 1
		if told != tc.told || strings.Contains(req.Continue.Brief, "use the release branch") != tc.told {
			t.Errorf("%s: the brief tells the kept lead %v and records %+v, want %v", name,
				strings.Contains(req.Continue.Brief, "use the release branch"), req.Continue.Told.Told, tc.told)
		}
	}
}

// A SEND-BACK'S KEPT CONTINUATION IS JUDGED TOO: a review's send-back continues
// the job's kept capture, and its verdict is asked on the review's lead.
//
// Mutation: hand a send-back the held telling alone, and this goes red.
func TestASendBacksKeptContinuationIsJudgedToo(t *testing.T) {
	t.Parallel()
	req := guarded{basis: keptClaude(near, false), bridged: []runner.BridgedCall{earlierCall},
		prep: copyOf(sandbox.HarnessEndOwnRun, near)}.relaunch(t)
	if req.Session == nil || !strings.Contains(req.Session.Replaced, prompts.MayCompactSession) {
		t.Errorf("the send-back's kept continuation is not judged: %+v", req.Session)
	}
}

// A CUT-OFF COPY THAT MAY COMPACT KEEPS ITS CUT-OFF WORDS: the record says it was
// cut off, which says more than that it may be a summary.
//
// Mutation: tell it the words of a session that may compact in their place, and
// this goes red.
func TestACutOffCopyThatMayCompactKeepsItsCutOffWords(t *testing.T) {
	t.Parallel()
	brief := inPlaceOf(t, guarded{basis: inBoxBasis, bridged: []runner.BridgedCall{earlierCall},
		carry: func(r *runner.Resume) { answered("use main")(r); copyOf(sandbox.HarnessEndCutOff, near)(r) }})
	if !strings.Contains(brief, prompts.InterruptedCopy) || strings.Contains(brief, prompts.MayCompactSession) {
		t.Errorf("a cut-off copy that may compact is not told it was cut off:\n%s", brief)
	}
}

// A KEPT CAPTURE CUT OFF SAYS WHETHER ITS JOB RESTARTED THE SESSION, apart from
// whether it may lack its brief: either way it is told its calls after its save
// point as calls it may already show; where its job restarted the session after
// the brief (its newest restart is not the one its launch handed it, or cannot
// be read), nothing before the restart survives but a summary, so every call of
// the round is told from its start.
//
// Mutation: set the restart from the record's cut-off word, or read a count that
// names no restart as one that did not restart, and a case goes red.
func TestAKeptCaptureCutOffSaysWhetherItsJobRestarted(t *testing.T) {
	t.Parallel()
	restarted := func(restart *string) *sandbox.ResumedCount {
		return &sandbox.ResumedCount{Tokens: 10_000, Restart: restart}
	}
	for name, tc := range map[string]struct {
		count   *sandbox.ResumedCount
		fromAll bool
	}{
		"no restart in its job": {count: restarted(new("b-1"))},
		"a restart in its job":  {count: restarted(new("b-2")), fromAll: true},
		"a restart not read":    {count: restarted(nil), fromAll: true},
	} {
		req := guarded{basis: keptClaude(tc.count, true), bridged: []runner.BridgedCall{earlierCall, laterCall},
			carry: func(r *runner.Resume) {
				answered("use main")(r)
				r.HarnessEnd, r.Count, r.Run.Harness = sandbox.HarnessEndCutOff, far, "claude-code"
				r.BasisRestart = new("b-1")
			}}.relaunch(t)
		if req.Session == nil {
			t.Fatalf("%s: the relaunch continues no kept session", name)
		}
		brief := req.Session.Replaced
		calls := strings.Index(brief, cutOffHeader)
		if calls < 0 || !strings.Contains(brief[calls:], "a later issue") {
			t.Errorf("%s: the calls after the save are not told as calls it may already show:\n%s", name, brief)
		}
		// HELD BELOW THE SAVE where its job did not restart: listed again under
		// words that say the session shows it, never among calls it may lack.
		// Read by section, since both calls name one tool and no digest tells
		// their lines apart. Where its job restarted, told from the round's
		// start and listed again nowhere.
		told, _, _ := strings.Cut(brief, listedAgainHeading)
		again, _ := sectionOf(brief, listedAgainHeading)
		if got := strings.Contains(told, "an earlier issue"); got != tc.fromAll {
			t.Errorf("%s: the call before the save is told %v, want %v:\n%s", name, got, tc.fromAll, brief)
		}
		if got := strings.Contains(again, "an earlier issue"); got == tc.fromAll {
			t.Errorf("%s: the call before the save is listed again %v, want %v:\n%s", name, got, !tc.fromAll,
				brief)
		}
	}
}

// THE NOTE CURSOR IS NEVER READ WHERE THE SESSION MAY COMPACT: a summary holds the
// notes its session read in its own words alone, so every note the turn has read
// is told again, as notes it may already show, whatever the capture's cursor.
//
// Mutation: read the cursor where the session may compact, and this goes red.
func TestTheNoteCursorIsNeverReadWhereTheSessionMayCompact(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	read := steer.Read{Note: steer.Note{ID: "first", Text: "the first note", By: "founder"},
		Reading: steer.Reading{Phase: types.PhaseExecute, Iteration: 1, Round: 1,
			Boundary: types.SteerBoundaryToolBatch, Launch: "job-1", Order: 1}}
	basis := keptClaude(near, false)
	basis.NoteCursor = 1
	relauncher := &probingLauncher{basis: basis}
	resume := &runner.Resume{State: parked.State, Steered: []steer.Read{read},
		Bridged: []runner.BridgedCall{earlierCall}}
	answered("use main")(resume)
	copyOf(sandbox.HarnessEndOwnRun, near)(resume)
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
		steer: steer.New(), resume: resume})
	if _, _, err := collector.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again := relauncher.last(t)
	if again.Session == nil || !strings.Contains(again.Session.Brief, notesAgainHeader) ||
		!strings.Contains(again.Session.Brief, prompts.SteerMessage("founder", "the first note")) {
		t.Errorf("the note its cursor holds is not told again to a session that may compact: %+v", again.Session)
	}
}

// EVERY TELLING OF A PATH READS ITS ONE VALUE. Each path decides once whether the
// session it continues may be a summary at its run's first request, and every
// part of its telling and its guard follows that value, never the record's word
// alone: an own-run copy in its job's box and a kept capture taken whole that
// their harness may compact are told as [toldAsASummary] asks; the same sessions
// far below their point are told as held ([toldAsHeld]).
//
// Mutation: read the record's word in place of the path's value in any one part
// (the copy's notes, the kept capture's leads, notes or calls), or arm the
// session's loads, and a case goes red.
func TestEveryTellingOfAPathReadsItsOneValue(t *testing.T) {
	t.Parallel()
	active := guarded{basis: inBoxBasis}.relaunch(t).Surface.Active()
	for name, c := range map[string]pathOf{
		"the copy in its box, its harness may compact it": {basis: inBoxBasis, copied: near, summary: true},
		"the copy in its box, far below its point":        {basis: inBoxBasis, copied: far},
		"a capture taken whole, its harness may compact it": {basis: keptClaude(near, false), kept: true,
			copied: far, summary: true},
		"a capture taken whole, far below its point": {basis: keptClaude(far, false), kept: true, copied: far},
	} {
		if told := c.told(t, active); c.summary {
			toldAsASummary(t, name, told)
		} else {
			toldAsHeld(t, name, told)
		}
	}
}

// A SESSION ITS JOB RESTARTED IS TOLD AS ONE THAT MAY BE A SUMMARY. A resume hands
// its model nothing before the newest restart of the session's main thread but
// what that restart left, so a job whose newest restart is not the one its launch
// handed it (a compaction as it worked, a microcompaction's boundary, a span
// replaced, a later root) may lack everything the engine counts it as holding:
// an own-run copy in its box and a kept capture taken whole are told as
// [toldAsASummary] asks however low their count, and their twins whose newest
// restart is their launch's as held.
//
// Mutation: leave the restart out of the copy's value, read it only of a capture
// cut off, or read a session that may be a summary as one its harness may
// compact alone, and a case goes red.
func TestASessionItsJobRestartedIsToldAsOneThatMayBeASummary(t *testing.T) {
	t.Parallel()
	active := guarded{basis: inBoxBasis}.relaunch(t).Surface.Active()
	restartedAt := func(restart string) *sandbox.ResumedCount {
		return &sandbox.ResumedCount{Tokens: 10_000, Restart: new(restart)}
	}
	for name, c := range map[string]pathOf{
		"the copy in its box, its job restarted it": {basis: inBoxBasis, copied: restartedAt("b-2"),
			basisAt: "b-1", summary: true},
		"the copy in its box, no restart in its job": {basis: inBoxBasis, copied: restartedAt("b-1"),
			basisAt: "b-1"},
		"a capture taken whole, its job restarted it": {basis: keptClaude(restartedAt("b-2"), false), kept: true,
			copied: restartedAt("b-1"), basisAt: "b-1", summary: true},
		"a capture taken whole, no restart in its job": {basis: keptClaude(restartedAt("b-1"), false), kept: true,
			copied: restartedAt("b-1"), basisAt: "b-1"},
	} {
		if told := c.told(t, active); c.summary {
			toldAsASummary(t, name, told)
		} else {
			toldAsHeld(t, name, told)
		}
	}
}
