package runner_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// AN OPENCODE SESSION'S FIRST REQUEST IS JUDGED BY ITS LAUNCH, in the box it
// gets: OpenCode compacts at a point its box sets for the run's model, which
// the runner cannot read, so where the session's count has a bound the runner
// builds both tellings and hands both, and the launch picks.

// keptOpenCode is the turn's kept session of an OpenCode job, saved at the
// round's first call, counted as count on the stub provider's reply.
func keptOpenCode(count *sandbox.ResumedCount, interrupted bool) runner.SessionBasis {
	basis := keptClaude(count, interrupted)
	basis.Harness, basis.CountProvider = sandbox.OpenCodeHarness, "stub"
	return basis
}

// openCodeCopy is what an OpenCode job's record says of the copy its box
// holds: its harness ended its own run, counted at count on the stub
// provider's reply, its restart its launch's.
func openCodeCopy(count *sandbox.ResumedCount) func(*runner.Resume) {
	return func(r *runner.Resume) {
		copyOf(sandbox.HarnessEndOwnRun, count)(r)
		r.Run.Harness, r.CountProvider = sandbox.OpenCodeHarness, "stub"
	}
}

// BY BOX BUILDS BOTH TELLINGS. An OpenCode session whose count has a bound is
// never judged against a point by the runner, however near Claude Code's
// threshold its count is: the held telling is picked, and the one told as a
// session that may be a summary rides beside it, with the count and its
// reply's provider the launch judges by, on the kept path and in place alike.
//
// Mutation: leave the summarized telling unbuilt under byBox, or judge an
// OpenCode count by Claude Code's window, and a case goes red.
func TestByBoxBuildsBothTellings(t *testing.T) {
	t.Parallel()
	kept := guarded{basis: keptOpenCode(near, false), bridged: []runner.BridgedCall{earlierCall},
		prep: openCodeCopy(near)}.relaunch(t)
	s := kept.Session
	switch {
	case s == nil:
		t.Fatal("the relaunch continues no kept session")
	case strings.Contains(s.Replaced, prompts.MayCompactSession):
		t.Errorf("the kept path's picked telling is the summarized one:\n%s", s.Replaced)
	case s.Summarized == nil || !strings.Contains(s.Summarized.Replaced, prompts.MayCompactSession) ||
		!strings.Contains(s.Summarized.Brief, prompts.MayCompactSession):
		t.Errorf("the kept path hands no summarized telling beside the held one: %+v", s.Summarized)
	case s.Count == nil || s.Count.Tokens != near.Tokens || s.CountProvider != "stub":
		t.Errorf("the kept path is judged by %+v on %q, want the capture's count on stub", s.Count,
			s.CountProvider)
	}
	inPlace := guarded{basis: inBoxBasis, bridged: []runner.BridgedCall{earlierCall},
		carry: func(r *runner.Resume) { answered("use main")(r); openCodeCopy(near)(r) }}.relaunch(t)
	c := inPlace.Continue
	switch {
	case c == nil || c.SessionID != "sess-1":
		t.Fatalf("the relaunch continues %+v in place, want the job's own copy", c)
	case strings.Contains(c.Brief, prompts.MayCompactSession):
		t.Errorf("the copy's picked brief is the summarized one:\n%s", c.Brief)
	case !strings.Contains(c.Summarized, prompts.MayCompactSession):
		t.Errorf("the copy is handed no summarized brief beside the held one:\n%s", c.Summarized)
	case c.Count == nil || c.Count.Tokens != near.Tokens || c.CountProvider != "stub":
		t.Errorf("the copy is judged by %+v on %q, want its record's count on stub", c.Count, c.CountProvider)
	}
}

// AN UNBOUNDED OPENCODE COUNT IS TOLD AS A SUMMARY WITH NO READ. A count with
// no bound (none at all, or a mark) cannot be proved whole by any point, so the
// runner settles it before any brief: the summarized telling alone, with no
// telling beside it for the launch to judge.
//
// Mutation: read an unbounded OpenCode count as the box's to judge, and a case
// goes red.
func TestAnUnboundedOpenCodeCountIsToldAsASummaryWithNoRead(t *testing.T) {
	t.Parallel()
	for name, count := range map[string]*sandbox.ResumedCount{
		"no count": nil,
		"a mark":   {Mark: sandbox.CountUnanchored, Restart: new("")},
	} {
		req := guarded{basis: inBoxBasis, bridged: []runner.BridgedCall{earlierCall},
			carry: func(r *runner.Resume) {
				answered("use main")(r)
				openCodeCopy(count)(r)
				r.BasisRestart = new("")
			}}.relaunch(t)
		c := req.Continue
		if !strings.Contains(c.Brief, prompts.MayCompactSession) || c.Summarized != "" {
			t.Errorf("%s: the copy is handed %q beside %q, want the summarized telling alone", name, c.Brief,
				c.Summarized)
		}
		kept := guarded{basis: keptOpenCode(count, false), bridged: []runner.BridgedCall{earlierCall},
			prep: openCodeCopy(count)}.relaunch(t)
		if s := kept.Session; s == nil || !strings.Contains(s.Replaced, prompts.MayCompactSession) ||
			s.Summarized != nil {
			t.Errorf("%s: the kept path hands %+v, want the summarized telling alone", name, s)
		}
	}
}

// THE GUARD OF A SUMMARIZED HAND-OFF ARMS NOTHING. Where the launch hands the
// summarized telling (its box's point did not prove the held one whole), the
// run's guard is the summarized telling's: no skill armed, so a skill-covered
// call is refused until the skill is loaded again; where it hands the held
// one, the skills its launch armed stand. The prompt published is the brief
// handed.
//
// Mutation: ignore the decision's Summarized, and a case goes red.
func TestTheGuardOfASummarizedHandoffArmsNothing(t *testing.T) {
	t.Parallel()
	active := guarded{basis: inBoxBasis}.relaunch(t).Surface.Active()
	for name, summarized := range map[string]bool{"the summarized brief": true, "the held brief": false} {
		g := guarded{basis: inBoxBasis, continued: true,
			state: execstate.State{ActiveTools: active, LoadedSkills: []string{"chat-conventions"}},
			carry: func(r *runner.Resume) { answered("use main")(r); openCodeCopy(far)(r) },
			decides: func(req runner.AgentRunRequest) runner.SessionDecision {
				return runner.SessionDecision{Continues: true, Summarized: summarized}
			}}
		req := g.relaunch(t)
		bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})
		refused := bridged[0].Failed && strings.Contains(bridged[0].Result, "chat-conventions")
		if refused != summarized {
			t.Errorf("%s: a skill-covered call refused %v, want %v: %q", name, refused, summarized,
				bridged[0].Result)
		}
	}
}

// AN OPENCODE RESTART IN THE JOB VOIDS WHAT ITS BRIEF TOLD HELD. An OpenCode
// count names its newest compaction as its restart, so a copy whose job
// compacted it after its brief (its restart is not the one its launch handed
// it) holds nothing before that restart but its summary: its held telling too
// is told every call of the round from its start, and its guard arms nothing.
// Its twin whose restart is its launch's is told as holding what its record
// says.
//
// Mutation: keep the restart's gate on Claude Code alone, and a case goes red.
func TestAnOpenCodeRestartInTheJobVoidsWhatItsBriefToldHeld(t *testing.T) {
	t.Parallel()
	active := guarded{basis: inBoxBasis}.relaunch(t).Surface.Active()
	for name, c := range map[string]struct {
		restart string
		summary bool
	}{
		"its job compacted it":     {restart: "msg_compacted", summary: true},
		"no compaction in its job": {restart: "msg_launch"},
	} {
		count := &sandbox.ResumedCount{Tokens: 10_000, Restart: new(c.restart)}
		told := continuedDown(t, guarded{basis: inBoxBasis, continued: true,
			bridged: []runner.BridgedCall{earlierCall},
			state:   execstate.State{ActiveTools: active, LoadedSkills: []string{"chat-conventions"}},
			carry: func(r *runner.Resume) {
				answered("use main")(r)
				openCodeCopy(count)(r)
				r.BasisRestart = new("msg_launch")
			}}, false)
		if got := strings.Contains(told.brief, prompts.MayCompactSession); got != c.summary {
			t.Errorf("%s: the copy's held brief is told as a summary %v, want %v:\n%s", name, got, c.summary,
				told.brief)
		}
		if told.refused != c.summary {
			t.Errorf("%s: its guard refuses a skill-covered call %v, want %v", name, told.refused, c.summary)
		}
	}
}

// A CLAUDE CODE CONTINUATION IS TOLD AS IT WAS. The launch's judgment in its
// box is OpenCode's alone: a Claude Code session is judged by its window here,
// as before, on both paths, and its continuation carries no summarized
// telling and no count for a launch to judge by; near its threshold it is
// told as a summary, far from it as held.
//
// Mutation: read Claude Code's first request as the box's to judge, and a
// case goes red.
func TestAClaudeCodeContinuationIsToldAsOnMain(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		count   *sandbox.ResumedCount
		summary bool
	}{"near its threshold": {count: near, summary: true}, "far from it": {count: far}} {
		kept := guarded{basis: keptClaude(tc.count, false), bridged: []runner.BridgedCall{earlierCall},
			prep: copyOf(sandbox.HarnessEndOwnRun, tc.count)}.relaunch(t)
		if s := kept.Session; s == nil || s.Summarized != nil || s.Count != nil || s.CountProvider != "" ||
			strings.Contains(s.Replaced, prompts.MayCompactSession) != tc.summary {
			t.Errorf("%s: the kept path hands %+v, want its window's verdict alone", name, s)
		}
		inPlace := guarded{basis: inBoxBasis, bridged: []runner.BridgedCall{earlierCall},
			carry: func(r *runner.Resume) {
				answered("use main")(r)
				copyOf(sandbox.HarnessEndOwnRun, tc.count)(r)
			}}.relaunch(t)
		if c := inPlace.Continue; c.Summarized != "" || c.Count != nil || c.CountProvider != "" ||
			strings.Contains(c.Brief, prompts.MayCompactSession) != tc.summary {
			t.Errorf("%s: the copy is handed %+v, want its window's verdict alone", name, c)
		}
	}
}
