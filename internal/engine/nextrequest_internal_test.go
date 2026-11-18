package engine

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// THE FLOOR READS THE NEXT REQUEST WHERE THE RECORD HOLDS ONE. A continued
// launch sends its session again before a word of its brief, and that request
// is the harness's own estimate of it: the last request's input with its reply
// and everything after it, restarted at a compaction. So a session whose
// capture or job's record holds that estimate is held to it, in a box of its
// own and in the job's own box alike, a compacted session to the smaller figure
// and never the larger, which is the compacted-away conversation's; and one
// that holds none (an older build's record, a stamp settled off the executor
// log) to its last request's input, as before. The job's record's estimate
// reaches the floor through the resume a person's answer drives.
//
// Mutation: read the last request's input whatever the record holds, and this
// goes red.
func TestTheFloorReadsTheNextRequestWhereTheRecordHoldsOne(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	l := &agentLauncher{engine: &Engine{}, seat: seat, turn: &turnctx.Turn{RunID: "run-1", Seat: seat}}
	member := harnessProvider(t, chainMember{key: "head", agent: executor.ClaudeCodeName, model: "claude-opus-5-5"})
	const last = 150_000
	for name, tc := range map[string]struct {
		next sandbox.NextRequest
		want int64
	}{
		"an estimate past the last request's input": {next: sandbox.NextRequest{Tokens: 160_000,
			OlderTokens: 130_000}, want: 160_000},
		"an estimate past a compaction": {next: sandbox.NextRequest{Tokens: 20_000}, want: 20_000},
		"no estimate":                   {want: last},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kept := runner.AgentRunRequest{Brief: "start", Session: &runner.SessionContinuation{Brief: "carry on",
				Basis: runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1",
					Harness: executor.ClaudeCodeName, ContextTokens: last, NextRequest: tc.next}}}
			if _, floor := l.continuedFloor(t.Context(), kept, member, true); floor.context != tc.want {
				t.Errorf("a kept capture's launch is held to a context of %d, want %d", floor.context, tc.want)
			}
			inPlace := runner.AgentRunRequest{Brief: "start", Continue: &runner.Continuation{SessionID: "s-1",
				Brief: "carry on", ContextTokens: last, NextRequest: tc.next}}
			if _, floor := l.continuedFloor(t.Context(), inPlace, member, true); floor.context != tc.want {
				t.Errorf("an in-place launch is held to a context of %d, want %d", floor.context, tc.want)
			}
		})
	}
	t.Run("the job's record, through its resume", func(t *testing.T) {
		t.Parallel()
		ctx := t.Context()
		const turnID = "t-next-request"
		read := newHeldTool("read_page")
		close(read.release)
		asking := launchBridged(t, turnID, read)
		asking.call(t, "read_page")
		row := asking.row(t, turnID)
		parkedOnAHeldCall(t, asking.pending, turnID, row.LaunchID)
		if _, ok, err := asking.pending.ClaimForResume(ctx, turnID, sandbox.RecordedAnswerTail(row.LaunchID),
			sandbox.Fence{}); err != nil || !ok {
			t.Fatalf("ClaimForResume = %v, %v", ok, err)
		}
		logged := asking.calls(t, turnID)
		resumed := asking.row(t, turnID)
		resumed.Launch.HarnessEnd = sandbox.HarnessEndOwnRun
		resumed.Launch.Context = &sandbox.SessionMeasure{Tokens: last, Source: sandbox.MeasureRequest,
			Launch: row.LaunchID}
		resumed.Launch.NextRequestTokens = 158_000
		handed := relaunchHanded(t, asking.launcher.company, asking.handle, asking.launcher.turn,
			runnerResume(resumeInput{Run: resumed, Bridged: logged,
				BridgedRead: logged.Appends(), State: execstate.State{Version: execstate.Version, Round: 1},
				Asked: &sandbox.AskedRun{Question: "which branch?", Answer: "use main", By: "founder",
					SessionID: "sess-ask", BoxKept: true}}, nil, nil, nil))
		if handed.Continue.NextRequest.Tokens != 158_000 {
			t.Fatalf("the relaunch hands its launch the next request %+v, want the record's 158,000",
				handed.Continue.NextRequest)
		}
		_, floor := asking.launcher.continuedFloor(ctx, *handed, member, true)
		if want := staticFloor(asking.launcher.company).continuing(158_000,
			harness.EstimateTokens(handed.Continue.Brief)); floor != want {
			t.Errorf("the launch is held to %+v, want %+v: the copy's next request with the brief", floor, want)
		}
	})
}

// THE FLOOR READS THE MEMBER'S RATE. Claude Code estimates text at four
// characters a token for fourteen older models and at three for every other, so
// a continued session's next request is held at the rate of the model the
// launch's member hands its harness, read off its canonical id however a
// platform or an opt-in decorates it; an alias, whose model this build cannot
// place, at three, the larger figure.
//
// Mutation: read the three-character figure for every model, and this goes red.
func TestTheFloorReadsTheMembersRate(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	l := &agentLauncher{engine: &Engine{}, seat: seat, turn: &turnctx.Turn{RunID: "run-1", Seat: seat}}
	next := sandbox.NextRequest{Tokens: 120_000, OlderTokens: 90_000}
	for model, want := range map[string]int64{
		"claude-haiku-4-5":           90_000,
		"claude-sonnet-4-5-20250929": 90_000,
		"claude-opus-4-20250514":     90_000,
		"claude-opus-4-1":            90_000,
		"claude-opus-4-6[1m]":        90_000,
		"claude-3-7-sonnet-latest":   90_000,
		"claude-opus-5-5":            120_000,
		"claude-opus-4-7":            120_000,
		"claude-sonnet-5":            120_000,
		"sonnet":                     120_000,
	} {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			member := harnessProvider(t, chainMember{key: "head", agent: executor.ClaudeCodeName, model: model})
			req := runner.AgentRunRequest{Brief: "start", Session: &runner.SessionContinuation{Brief: "carry on",
				Basis: runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1",
					Harness: executor.ClaudeCodeName, ContextTokens: 10_000, NextRequest: next}}}
			if _, floor := l.continuedFloor(t.Context(), req, member, true); floor.context != want {
				t.Errorf("a launch on %s is held to a context of %d, want %d", model, floor.context, want)
			}
		})
	}
}

// THE FLOOR COUNTS THE SUMMARIZED BRIEF. A launch that judges an OpenCode
// session's first request in its box may hand it the brief told as a session
// that may be a summary, the longer one, so the floor is the longest of every
// brief the launch may hand, on the kept path and in place alike; and the
// summarized brief is never dropped alone: a session whose summarized brief no
// window carries is not continued, rather than continued with the held brief a
// box may refuse to prove whole.
//
// Mutation: leave the summarized brief out of either floor, or drop it alone,
// and a case goes red.
func TestTheFloorCountsTheSummarizedBrief(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	l := &agentLauncher{engine: &Engine{}, seat: seat, turn: &turnctx.Turn{RunID: "run-1", Seat: seat}}
	member := harnessProvider(t, chainMember{key: "head", agent: executor.OpenCodeName, model: "stub/m"})
	summarized := strings.Repeat("told again ", 4000)
	want := harness.EstimateTokens(summarized)
	kept := runner.AgentRunRequest{Brief: "start", Session: &runner.SessionContinuation{Brief: "carry on",
		Replaced: "carry on elsewhere", Summarized: &runner.SummarizedTelling{Brief: "short", Replaced: summarized},
		Basis: runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1", Harness: executor.OpenCodeName,
			ContextTokens: 10_000}}}
	if _, floor := l.continuedFloor(t.Context(), kept, member, true); floor.brief != want {
		t.Errorf("a kept capture's launch is held to a brief of %d, want the summarized one's %d", floor.brief,
			want)
	}
	inPlace := runner.AgentRunRequest{Brief: "start", Continue: &runner.Continuation{SessionID: "s-1",
		Brief: "carry on", Summarized: summarized, ContextTokens: 10_000}}
	if _, floor := l.continuedFloor(t.Context(), inPlace, member, true); floor.brief != want {
		t.Errorf("an in-place launch is held to a brief of %d, want the summarized one's %d", floor.brief, want)
	}
	claude := harnessProvider(t, chainMember{key: "head", agent: executor.ClaudeCodeName, model: "claude-opus-5-5"})
	huge := kept
	session := *kept.Session
	session.Basis.Harness = executor.ClaudeCodeName
	session.Summarized = &runner.SummarizedTelling{Brief: "short", Replaced: strings.Repeat("x", 4_000_000)}
	huge.Session = &session
	if got, _ := l.continuedFloor(t.Context(), huge, claude, true); got.Session != nil {
		t.Errorf("a session whose summarized brief no window carries is continued with %+v, want not continued",
			got.Session)
	}
}
