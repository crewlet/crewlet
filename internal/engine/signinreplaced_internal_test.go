package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A RUN REFUSED AFTER A ROTATION IS RELAUNCHED ONCE: a job that kept the
// replaced credential is carried on with the new one rather than failed. Not
// when the refusal is on the sign-in still in use, the job is itself that
// relaunch (a credential that keeps failing fails the turn), the ending is not
// an auth failure, or a person's answer or a lost host already carries it on.
//
// Mutation: drop any one condition from signInReplaced and a row fails.
func TestARunRefusedAfterARotationIsRelaunchedOnce(t *testing.T) {
	t.Parallel()
	c, _ := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus"})
	current := string(planOf(t, c).SignInID())
	if current == "" {
		t.Fatal("the premise: the entry has a sign-in identity")
	}
	refused := sandbox.RunEnding{Text: "Failed to authenticate. API Error: 401 {\"type\":\"error\"}",
		Error: "Failed to authenticate. API Error: 401", At: time.Now()}
	answered := sandbox.RunEnding{Success: true, Text: "fixed the flake", At: time.Now()}
	run := func(signIn string, followUp sandbox.FollowUpReason) sandbox.PendingRun {
		return sandbox.PendingRun{TurnID: "t0", AgentHandle: "swe", SessionID: "sess-1", SandboxID: "box-1",
			Launch: sandbox.LaunchRecord{ProviderKey: "plan", SignInID: signIn, FollowUp: followUp}}
	}
	for name, tc := range map[string]struct {
		in       resumeInput
		relaunch bool
	}{
		"refused on a replaced sign-in": {resumeInput{Run: run("an-older-sign-in", ""), Ended: refused}, true},
		"refused on the sign-in in use": {resumeInput{Run: run(current, ""), Ended: refused}, false},
		"refused again after the relaunch": {resumeInput{Run: run("an-older-sign-in", sandbox.FollowUpSignInReplaced),
			Ended: refused}, false},
		"no sign-in recorded at launch":     {resumeInput{Run: run("", ""), Ended: refused}, false},
		"answered on a replaced sign-in":    {resumeInput{Run: run("an-older-sign-in", ""), Ended: answered}, false},
		"a person's answer carries it on":   {resumeInput{Run: run("an-older-sign-in", ""), Ended: refused, Asked: &sandbox.AskedRun{}}, false},
		"a lost host carries it on already": {resumeInput{Run: run("an-older-sign-in", ""), Ended: refused, Lost: &sandbox.LostRun{}}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := (&Engine{}).signInReplaced(t.Context(), c, tc.in)
			if (got != nil) != tc.relaunch {
				t.Fatalf("signInReplaced = %+v, want relaunch %v", got, tc.relaunch)
			}
			if got != nil && (*got != runner.Rotated{SessionID: "sess-1", BoxKept: true}) {
				t.Errorf("the carried round = %+v, want the job's own session in its kept box", *got)
			}
		})
	}
}

// A ROUND A REPLACED SIGN-IN CUT OFF IS CARRIED ON IN THE SESSION ITS JOB'S BOX
// RAN: a job whose box could not take up the session its launch chose started
// its own, whose transcript holds what the job did and the skills it loaded.
//
// Mutation: carry the round on in the session the launch chose, and the
// relaunch continues a session that never read what its guard is armed with.
func TestARotatedRoundIsCarriedOnInTheSessionItsBoxRan(t *testing.T) {
	t.Parallel()
	c, _ := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus"})
	refused := sandbox.RunEnding{Text: "Failed to authenticate. API Error: 401 {\"type\":\"error\"}",
		Error: "Failed to authenticate. API Error: 401", At: time.Now()}
	run := sandbox.PendingRun{TurnID: "t0", AgentHandle: "swe", SessionID: "sess-chosen", SandboxID: "box-1",
		Launch: sandbox.LaunchRecord{ProviderKey: "plan", SignInID: "an-older-sign-in",
			FreshSessionID: "sess-own", SessionStart: sandbox.SessionStartFresh}}
	got := (&Engine{}).signInReplaced(t.Context(), c, resumeInput{Run: run, Ended: refused})
	if got == nil || got.SessionID != "sess-own" {
		t.Fatalf("the carried round = %+v, want it in the session the job's box started of its own", got)
	}
}

// THE LAUNCH STAMPS THE SIGN-IN THE JOB'S HARNESS IS HANDED on the job's record,
// so whichever node collects the job can tell a refusal on a replaced
// credential from an ordinary one.
//
// Mutation: leave SignInID out of the launch request, and this fails.
func TestALaunchRecordsTheSignInItsJobWasHanded(t *testing.T) {
	t.Parallel()
	c, seat := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus"})
	e, _ := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	l := launcherOn(e, c, seat, "t1")
	var handoffs []chain.Fallback
	if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, &handoffs)); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	rt := e.sandbox.Load()
	row, found, err := rt.pending.Get(t.Context(), "t1")
	if err != nil || !found {
		t.Fatalf("the run's row: found %v, %v", found, err)
	}
	if want := string(planOf(t, c).SignInID()); row.Launch.SignInID != want {
		t.Errorf("the job's record names sign-in %q, want the entry's %q", row.Launch.SignInID, want)
	}
}

// A RUN'S ENDING IS THE SIGN-IN'S IT LAUNCHED WITH: a run on a since-replaced
// credential is not the new sign-in's failure, readiness verdict or limit; a
// run on the sign-in in use records as always.
//
// Mutation: record every ending on the entry whatever it launched with, and
// the refused run becomes the new sign-in's failure, verdict and limit.
func TestARunsEndingIsTheSignInsItLaunchedWith(t *testing.T) {
	t.Parallel()
	// DATED WHEN EACH CASE RUNS, not when the table is built: a parallel case
	// can start minutes later, past the window a limit's report opens.
	refused := func() sandbox.RunEnding {
		return sandbox.RunEnding{Text: "Failed to authenticate. API Error: 401 {\"type\":\"error\"}",
			Error: "Failed to authenticate. API Error: 401", At: time.Now()}
	}
	spent := func() sandbox.RunEnding {
		return sandbox.RunEnding{Error: "You've hit your 5-hour limit", Text: "You've hit your 5-hour limit",
			Event: `{"type":"result","is_error":true,"api_error_status":429,"result":"You've hit your 5-hour limit"}`,
			At:    time.Now()}
	}
	for name, ended := range map[string]func() sandbox.RunEnding{"a refusal": refused, "a limit": spent} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ending := ended()
			c, _ := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus"})
			stale := sandbox.PendingRun{TurnID: "t0", AgentHandle: "swe",
				Launch: sandbox.LaunchRecord{ProviderKey: "plan", SignInID: "an-older-sign-in"}}
			e := epochEngine(c)
			e.noteRunEnded(t.Context(), c, stale, ending)
			state := planOf(t, c).Plan().State(time.Now())
			if state.LastFailure != nil || len(state.SittingOut) != 0 {
				t.Fatalf("an ending on a replaced sign-in landed on the current one: %+v", state)
			}
			if err := planOf(t, c).Unready(t.Context()); err != nil {
				t.Fatalf("the current sign-in reads unready after another's ending: %v", err)
			}
			if rot := e.signInReplaced(t.Context(), c, resumeInput{Run: stale, Ended: ending}); name == "a refusal" && rot == nil {
				t.Error("the refused run is not carried on with the new sign-in")
			}

			current := stale
			current.Launch.SignInID = string(planOf(t, c).SignInID())
			e.noteRunEnded(t.Context(), c, current, ending)
			if after := planOf(t, c).Plan().State(time.Now()); after.LastFailure == nil && len(after.SittingOut) == 0 {
				t.Errorf("an ending on the sign-in in use recorded nothing: %+v", after)
			}
		})
	}
}
