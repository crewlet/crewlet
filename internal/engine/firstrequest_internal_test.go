package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE PROBE JUDGES UNDER THE CHAIN'S SMALLEST STANDARD WINDOW: the launch picks its
// member only as it launches, so a continuation's telling is judged under the
// least window Claude Code holds any Claude Code member's model to, on the route
// each reaches it by: a login, a key and the host's own environment Anthropic's
// own API, an entry signed in by its env a route this build reads as the least.
// Known only where every member's model is; an OpenCode member is no part of it.
//
// Mutation: take the first member's window, or read an inherit-env entry as a
// route of its own, and a case goes red.
func TestTheProbeJudgesUnderTheChainsSmallestStandardWindow(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		members []chainMember
		want    sandbox.ExecutorWindow
	}{
		"a first-party sonnet, then claude-sonnet-4-6": {members: []chainMember{
			{key: "a", agent: "claude-code", model: "sonnet"},
			{key: "b", agent: "claude-code", model: "claude-sonnet-4-6"}},
			want: sandbox.ExecutorWindow{Tokens: 1_000_000, Standard: 200_000, Known: true}},
		"an OpenCode member beside a first-party opus": {members: []chainMember{
			{key: "oc", agent: "opencode", model: "anthropic/claude-sonnet-5"},
			{key: "a", agent: "claude-code", model: "opus"}},
			want: sandbox.ExecutorWindow{Tokens: 1_000_000, Standard: 1_000_000, Known: true}},
		"a model this build does not know": {members: []chainMember{
			{key: "a", agent: "claude-code", model: "opus"},
			{key: "b", agent: "claude-code", model: "claude-golden"}},
			want: sandbox.ExecutorWindow{Tokens: 1_000_000, Standard: 1_000_000}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, seat := chainCompany(t, tc.members...)
			e, _ := launchRigRestoring(t, c, bridgeFor(t, mcpbridge.Options{
				Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
			}), &keptLog{rows: map[string]executordomain.Session{}, archives: map[string][]byte{}})
			if got := launcherOn(e, c, seat, "t1").continuationWindow(); got != tc.want {
				t.Errorf("the probe's window is %+v, want %+v", got, tc.want)
			}
		})
	}
	for mode, want := range map[harness.AuthMode]sandbox.ClaudeRoute{
		harness.AuthSubscription: sandbox.ClaudeRouteAnthropic, harness.AuthAPIKey: sandbox.ClaudeRouteAnthropic,
		harness.AuthInheritEnv: sandbox.ClaudeRouteAnthropic, harness.AuthEnv: sandbox.ClaudeRouteElsewhere,
		harness.AuthMode("a-later-builds-mode"): sandbox.ClaudeRouteElsewhere,
	} {
		if got := claudeRoute(mode); got != want {
			t.Errorf("an entry signed in by %q is judged on the route %q, want %q", mode, got, want)
		}
	}
}

// EVERY BASIS THE PROBE ANSWERS CARRIES THE WINDOW, whatever branch answered it:
// the in-place path is judged by it where no kept session is continued; and a
// node with no sandbox runtime answers an unknown window, never a panic on the
// manager it does not have.
//
// Mutation: set the window on the kept branch alone, or ask a chain with no
// manager, and a case goes red.
func TestEveryBasisTheProbeAnswersCarriesTheWindow(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	unknown := sandbox.ExecutorWindow{Tokens: sandbox.FallbackExecutorWindow, Standard: sandbox.FallbackExecutorWindow}
	noRuntime := launcherOn(&Engine{}, c, seat, "t1").ProbeSession(t.Context())
	if noRuntime.Window != (sandbox.ExecutorWindow{}) || noRuntime.Why == "" {
		t.Errorf("a node with no sandbox runtime answers %+v, want an unknown window and why", noRuntime)
	}
	k := newKeptLaunch(t, 40000, 0, 0)
	if got := k.l.ProbeSession(k.ctx); !got.Continues() || got.Window != unknown {
		t.Errorf("the kept branch answers %+v, want the chain's window", got)
	}
	nothingKept := launcherOn(k.e, k.l.company, k.l.seat, "t-nothing-kept")
	if got := nothingKept.ProbeSession(k.ctx); got.Continues() || got.Window != unknown {
		t.Errorf("a turn that kept nothing answers %+v, want the chain's window beside why", got)
	}
}

// A PROBE OF A CAPTURE PASSES ITS COUNT: what the capture's take counted its
// archive at is what the kept path's first request is judged by.
//
// Mutation: leave the count off the basis, and this goes red.
func TestAProbeOfACapturePassesItsCount(t *testing.T) {
	t.Parallel()
	count := &sandbox.ResumedCount{Tokens: 42_000, Window: 200_000, Restart: new("u-1")}
	k := newKeptLaunchCounted(t, 40000, 0, 0, count)
	got := k.l.ProbeSession(k.ctx).Count
	if got == nil || got.Tokens != count.Tokens || got.Window != count.Window || got.Restart == nil ||
		*got.Restart != "u-1" {
		t.Errorf("the probe passes the count %+v, want the capture's %+v", got, count)
	}
}

// A RESUME CARRIES THE JOB'S COUNT AND ITS RESTART BASIS off the job's own record:
// what its collection counted the copy in its box at, and the restart its launch
// handed it, a thread that restarts nowhere where its box said it started a
// session of its own.
//
// Mutation: drop either off the resume, and a case goes red.
func TestRunnerResumeCarriesTheJobsCount(t *testing.T) {
	t.Parallel()
	count := &sandbox.ResumedCount{Tokens: 42_000, Restart: new("u-9")}
	got := runnerResume(resumeInput{Run: sandbox.PendingRun{Launch: sandbox.LaunchRecord{ResumedCount: count,
		BasisRestart: new("u-1")}}}, nil, nil, nil)
	if got.Count != count || got.BasisRestart == nil || *got.BasisRestart != "u-1" {
		t.Errorf("the resume carries the count %+v and the basis %v, want the record's", got.Count, got.BasisRestart)
	}
	fresh := runnerResume(resumeInput{Run: sandbox.PendingRun{Launch: sandbox.LaunchRecord{
		BasisRestart: new("u-1"), SessionStart: sandbox.SessionStartFresh}}}, nil, nil, nil)
	if fresh.BasisRestart == nil || *fresh.BasisRestart != "" {
		t.Errorf("a job whose box started a session of its own carries the basis %v, want none restarted",
			fresh.BasisRestart)
	}
}

// probingRecorder is a [relaunchRecorder] that answers the probe with basis.
type probingRecorder struct {
	relaunchRecorder
	basis runner.SessionBasis
}

func (p *probingRecorder) ProbeSession(context.Context) runner.SessionBasis { return p.basis }

// AN IN-BOX CONTINUATION IS JUDGED BY ITS COLLECTION'S COUNT, end to end from the
// job's record: a copy whose harness ended its own run, counted near its
// threshold under a claude-sonnet-4-5 member's window, is told its round again
// as one that may compact; its twin counted far below is told as held, which
// only a recorded bound reaching the runner produces.
//
// Mutation: drop the record's count on its way to the runner, and the twin far
// below is told as one that may compact.
func TestAnInBoxContinuationIsJudgedByItsCollectionsCount(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		tokens   int64
		compacts bool
	}{
		"near its threshold":      {tokens: 160_000, compacts: true},
		"far below its threshold": {tokens: 10_000},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, seat := executorCompany(t)
			recorder := &probingRecorder{basis: runner.SessionBasis{Why: "the turn keeps no session",
				Window: sandbox.ClaudeCodeWindow("claude-sonnet-4-5")}}
			r, err := c.RunnerFor(t.Context(), seat.Handle(), tools.NewRegistry(), RunnerInput{
				Task: "fix the failing test", Reply: turn.ToolReply(""), Launcher: recorder,
				Sessions: testSessions(t),
				Resume: runnerResume(resumeInput{
					Run: sandbox.PendingRun{Harness: sandbox.ClaudeCodeHarness, Launch: sandbox.LaunchRecord{
						HarnessEnd: sandbox.HarnessEndOwnRun, BasisRestart: new(""),
						ResumedCount: &sandbox.ResumedCount{Tokens: tc.tokens, Restart: new("")}}},
					State: execstate.State{Version: execstate.Version, Round: 1},
					Asked: &sandbox.AskedRun{Question: "which branch?", Answer: "use main", By: "founder",
						SessionID: "sess-ask", BoxKept: true}}, nil, nil, nil),
				Turn: runner.Turn{RunID: "t1", Context: &turnctx.Turn{RunID: "t1", Seat: seat, Org: c.Org}},
			})
			if err != nil {
				t.Fatalf("RunnerFor: %v", err)
			}
			_, _, _ = r.Resume(t.Context(), nil)
			handed := recorder.handed()
			if handed == nil || handed.Continue == nil {
				t.Fatalf("the relaunch continues no copy in its job's box: %+v", handed)
			}
			if got := strings.Contains(handed.Continue.Brief, prompts.MayCompactSession); got != tc.compacts {
				t.Errorf("the copy is told it may compact %v, want %v:\n%s", got, tc.compacts, handed.Continue.Brief)
			}
		})
	}
}

// A CAPTURED SESSION IS JUDGED BY ITS CAPTURE'S COUNT, end to end from the
// capture's stamp through the probe: a kept capture its harness took whole,
// counted near its threshold under a claude-sonnet-4-5 member's window, is told
// its round again as one that may compact; its twin counted far below is told
// as held, which only the stamp's bound reaching the runner through the probe
// produces. The copy in the job's box is counted far below in both, as the
// read that collected the job recorded it on the job's record, since a capture
// taken whole is judged by the worse of the two.
//
// Mutation: leave the stamp's count off the probe's basis, and the twin far
// below is told as one that may compact.
func TestACapturedSessionIsJudgedByItsCapturesCount(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		tokens   int64
		compacts bool
	}{
		"near its threshold":      {tokens: 160_000, compacts: true},
		"far below its threshold": {tokens: 10_000},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, seat := chainCompany(t, chainMember{key: "api", agent: "claude-code", model: "claude-sonnet-4-5"})
			k := newKeptLaunchOn(t, c, seat, 40000, 0, 0, &sandbox.ResumedCount{Tokens: tc.tokens, Restart: new("")},
				false)
			pending := k.e.sandbox.Load().pending
			row, _, err := pending.Get(k.ctx, "t1")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if ok, err := pending.MarkHarnessEnd(k.ctx, "t1", row.LaunchID, sandbox.EndRead{
				End: sandbox.HarnessEndOwnRun, Count: &sandbox.ResumedCount{Tokens: 10_000, Restart: new("")},
			}); err != nil || !ok {
				t.Fatalf("recording the job's own copy's count: %v, %v", ok, err)
			}
			if row, _, err = pending.Get(k.ctx, "t1"); err != nil {
				t.Fatalf("Get: %v", err)
			}
			recorder := &probingRecorder{basis: k.l.ProbeSession(k.ctx)}
			r, err := c.RunnerFor(k.ctx, seat.Handle(), tools.NewRegistry(), RunnerInput{
				Task: "fix the failing test", Reply: turn.ToolReply(""), Launcher: recorder,
				Sessions: testSessions(t),
				Resume: runnerResume(resumeInput{Run: row, State: execstate.State{Version: execstate.Version, Round: 1},
					Asked: &sandbox.AskedRun{Question: "which branch?", Answer: "use main", By: "founder",
						SessionID: "s-1", BoxKept: true}}, nil, nil, nil),
				Turn: runner.Turn{RunID: "t1", Context: k.l.turn},
			})
			if err != nil {
				t.Fatalf("RunnerFor: %v", err)
			}
			_, _, _ = r.Resume(k.ctx, nil)
			handed := recorder.handed()
			if handed == nil || handed.Session == nil {
				t.Fatalf("the relaunch continues no kept session: %+v", handed)
			}
			if got := strings.Contains(handed.Session.Replaced, prompts.MayCompactSession); got != tc.compacts {
				t.Errorf("the capture is told it may compact %v, want %v:\n%s", got, tc.compacts,
					handed.Session.Replaced)
			}
		})
	}
}

// envSigned is a real Claude Code provider of model, signed in by its entry's own
// env, env.
func envSigned(t *testing.T, model string, env map[string]string) *harness.Provider {
	t.Helper()
	provider, err := harness.New(harness.Config{Key: "env", Harness: "claude-code", Model: model,
		StateDir: t.TempDir(), Overrides: map[string]any{"binary": filepath.Join(t.TempDir(), "no-cli-here")},
		Timeout: time.Minute, Auth: harness.Auth{Mode: harness.AuthEnv}, Env: env})
	if err != nil {
		t.Fatalf("harness.New: %v", err)
	}
	return provider
}

// THE FLOOR JUDGES A MEMBER UNDER THE WINDOW CLAUDE CODE HOLDS IT TO: the pin, held
// to the least window Claude Code holds the member's model to on the member's own
// platform, read exactly off its entry: claude-opus-4-6 and claude-sonnet-4-6 at
// 200,000 by default and at 1,000,000 where the entry opts in with `[1m]`, no
// model at 200,000, an alias at the window of the model it names on the entry's
// cloud platform (`opus` claude-opus-5-5 on Bedrock, claude-opus-4-6 on Foundry),
// and a switch whose value Claude Code does not read as true or false judged as
// the least. A session kept under a larger window is not continued on a member
// one request of whose model cannot carry it.
//
// Mutation: judge under the pin, or read every env entry's platform as the
// least, and a case goes red.
func TestTheFloorJudgesAMemberUnderTheWindowClaudeCodeHoldsItTo(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		member *harness.Provider
		want   int
	}{
		"claude-opus-4-6 on a key": {member: harnessProvider(t, chainMember{key: "a", agent: "claude-code",
			model: "claude-opus-4-6"}), want: 200_000},
		"claude-opus-4-6, opted in": {member: harnessProvider(t, chainMember{key: "a", agent: "claude-code",
			model: "claude-opus-4-6[1m]"}), want: 1_000_000},
		"no model": {member: harnessProvider(t, chainMember{key: "a", agent: "claude-code"}), want: 200_000},
		"opus on a key": {member: harnessProvider(t, chainMember{key: "a", agent: "claude-code", model: "opus"}),
			want: 1_000_000},
		"opus on Bedrock": {member: envSigned(t, "opus", map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1",
			"AWS_REGION": "us-east-1"}), want: 1_000_000},
		"opus on Foundry": {member: envSigned(t, "opus", map[string]string{"CLAUDE_CODE_USE_FOUNDRY": "true"}),
			want: 200_000},
		"sonnet on Vertex": {member: envSigned(t, "sonnet", map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"}),
			want: 200_000},
		"opus behind a switch read as neither": {member: envSigned(t, "opus", map[string]string{
			"CLAUDE_CODE_USE_BEDROCK": "perhaps"}), want: 200_000},
	} {
		if got := floorWindow(tc.member).Tokens; got != tc.want {
			t.Errorf("%s: the floor judges the member under %d, want %d", name, got, tc.want)
		}
	}
	kept := launchFloor{context: 300_000}
	if why := uncarried(harnessProvider(t, chainMember{key: "a", agent: "claude-code", model: "claude-opus-4-6"}),
		windowRoom{}, kept); why == "" {
		t.Error("a session of 300,000 tokens is continued on a claude-opus-4-6 member")
	}
	if why := uncarried(harnessProvider(t, chainMember{key: "a", agent: "claude-code", model: "opus"}),
		windowRoom{}, kept); why != "" {
		t.Errorf("a session of 300,000 tokens is not continued on an opus member: %s", why)
	}
}
