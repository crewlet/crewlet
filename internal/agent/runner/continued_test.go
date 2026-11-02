package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/skills"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// probingLauncher is a [playLauncher] that says, as an engine's launcher does,
// which kept session of the turn a relaunch continues ([runner.SessionProber]).
type probingLauncher struct {
	playLauncher
	basis runner.SessionBasis
	// probed is the brief size each probe was asked with, and carried
	// whether it was asked for a relaunch that carries its round on.
	probed  []int
	carried []bool
}

func (l *probingLauncher) ProbeSession(_ context.Context, briefTokens int, carries bool) runner.SessionBasis {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.probed = append(l.probed, briefTokens)
	l.carried = append(l.carried, carries)
	return l.basis
}

// keptBasis is a turn's kept session as a launcher answers it: captured once
// round 1's run had made its one call and read its first note.
var keptBasis = runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1",
	BridgeSeq: 1, NoteCursor: 1, Home: "/boxes/4f2a/home", ContextTokens: 40000}

// ledgerOf is the calls of a run as the turn's closed round records them.
func ledgerOf(bridged []runner.BridgedCall) []ledger.Call {
	out := make([]ledger.Call, 0, len(bridged))
	for _, c := range bridged {
		out = append(out, c.Call)
	}
	return out
}

// A SENT-BACK SESSION IS TOLD WHAT IT LACKS. A reviewer's send-back continues
// the session that did the round, so its brief is that session's next message:
// the task restated — a compacted session holds only its own summary of it —
// the rules its earlier rounds' calls are read by, without their list, which
// the session holds; what the review asked of the next round; and every note
// the turn has read that the session has not — the one the reviewer read
// after the session was saved — then the one this brief is the first to read,
// never one the session read itself. In a box that replaced the session's, it
// is told that too: a fresh checkout, and the old box's paths. The fresh brief
// beside it still carries every note.
//
// Mutation: carry only the notes this brief is the first to read, or every
// note the turn read, or drop the task or the rules, and this goes red.
func TestASentBackSessionIsToldWhatItLacks(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"first pass"}`)}})
	parked, _ := launched.Suspended()

	read := steer.Note{ID: "read", Text: "use the staging database", By: "founder"}
	reviewed := steer.Note{ID: "reviewed", Text: "and keep the old flag", By: "founder"}
	fresh := steer.Note{ID: "fresh", Text: "and tag the release", By: "founder"}
	box := steer.New()
	box.Offer(fresh)
	relauncher := &probingLauncher{basis: keptBasis}
	collector, _ := buildWith(t, entries, buildOpts{
		calls: turnctx.NewCallLog(), launcher: relauncher, steer: box,
		resume: &runner.Resume{State: parked.State, Bridged: bridged, Answer: "first pass done",
			Steered: []steer.Read{
				{Note: read, Reading: steer.Reading{Phase: types.PhaseExecute, Iteration: 1, Round: 1,
					Boundary: types.SteerBoundaryToolBatch, Launch: "job-1", Order: 1}},
				{Note: reviewed, Reading: steer.Reading{Phase: types.PhaseReview, Iteration: 1, Round: 1,
					Boundary: types.SteerBoundaryRound, Order: 2}},
			}},
	})
	if _, _, err := collector.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	history := []ledger.Iteration{{Iteration: 1, Calls: ledgerOf(bridged)}}
	if _, _, err := collector.Execute(t.Context(), 2, "the test still fails on CI", history); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	again := relauncher.last(t)
	if again.Session == nil || again.Session.Basis.SessionID != "s-1" {
		t.Fatalf("the send-back continues %+v, want the turn's kept session", again.Session)
	}
	brief := again.Session.Brief
	for _, want := range []string{"## Your work was reviewed and sent back", "post the weekly summary",
		"ALREADY RAN", "the test still fails on CI", prompts.SteerMessage(reviewed.By, reviewed.Text),
		prompts.SteerMessage(fresh.By, fresh.Text)} {
		if !strings.Contains(brief, want) {
			t.Errorf("the continued session is not told %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, prompts.SteerMessage(read.By, read.Text)) {
		t.Errorf("the continued session is handed a note it read itself:\n%s", brief)
	}
	if strings.Contains(brief, "slack_post") {
		t.Errorf("the continued session is handed its earlier round's calls, which it holds:\n%s", brief)
	}
	if !strings.HasPrefix(again.Session.Replaced, brief) ||
		!strings.Contains(again.Session.Replaced, "FRESH checkout") ||
		!strings.Contains(again.Session.Replaced, keptBasis.Home) {
		t.Errorf("the new-box continuation does not say what a replaced box changes:\n%s",
			again.Session.Replaced)
	}
	for _, note := range []steer.Note{read, reviewed, fresh} {
		if !strings.Contains(again.Brief, prompts.SteerMessage(note.By, note.Text)) {
			t.Errorf("the fresh brief does not carry note %q", note.ID)
		}
	}
	if len(relauncher.probed) != 1 || relauncher.probed[0] <= 0 || relauncher.carried[0] {
		t.Errorf("the launcher was asked %v (carrying a round on: %v), want once, with the fresh brief's "+
			"size, for a send-back that begins a round", relauncher.probed, relauncher.carried)
	}
}

// A CARRIED-ON SESSION IS TOLD WHAT CAME AFTER IT WAS SAVED. A lost job's round
// continues the turn's kept session, saved before the job was cut off: so it is
// told it was cut off, that its transcript may end mid-call, and every call the
// run made through the seat's tools after the point the session ends, with the
// call's outcome — never one the session holds. A round lost a second time
// starts a session of its own, rather than replay the state it was lost in
// toward the round's limit.
//
// Mutation: list every call of the round, drop the interrupted note, or
// continue the session after a second loss, and a row goes red.
func TestACarriedOnSessionIsToldWhatCameAfterItWasSaved(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	bridged := play(t, req, run{calls: []llm.ToolCall{
		call(t, "slack_post", `{"text":"starting on it"}`),
		call(t, "jira_create", `{"summary":"after the save"}`),
	}})
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	basis := keptBasis
	basis.Interrupted = true

	carriedOn := func(losses int) runner.AgentRunRequest {
		t.Helper()
		relauncher := &probingLauncher{basis: basis}
		lost := runner.Lost{Why: "the engine it ran under restarted, which killed it", BoxKept: true,
			Losses: losses, Limit: 2}
		collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
			resume: &runner.Resume{State: parked.State, Bridged: bridged, Lost: &lost}})
		if _, _, err := collector.Resume(t.Context(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if losses <= 1 && (len(relauncher.carried) != 1 || !relauncher.carried[0]) {
			t.Errorf("the launcher was asked %v, want once, for a relaunch that carries its round on",
				relauncher.carried)
		}
		return relauncher.last(t)
	}

	first := carriedOn(1)
	if first.Session == nil {
		t.Fatal("a lost job's round continues no kept session")
	}
	brief := first.Session.Brief
	for _, want := range []string{"## Your previous segment was cut off", prompts.InterruptedSession,
		"## Calls made after the session above was saved", "jira_create"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the carried-on session is not told %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "slack_post") {
		t.Errorf("the carried-on session is handed a call it holds:\n%s", brief)
	}

	if second := carriedOn(2); second.Session != nil {
		t.Errorf("a round lost a second time continues %+v, want a session of its own", second.Session)
	}
}

// guardedRelaunch is a seat whose surface carries one required skill covering
// slack_post, relaunching its executor in round 2 of a turn whose round 1
// loaded that skill, with basis as what its launcher says the relaunch
// continues.
func guardedRelaunch(t *testing.T, basis runner.SessionBasis) runner.AgentRunRequest {
	t.Helper()
	registry := skills.NewRegistry()
	if _, err := registry.PutPage(skills.Skill{
		Key: "chat-conventions", Title: "Chat conventions", Summary: "how this company writes on chat",
		Body: "Always thread your reply.", Trigger: skills.Trigger{Tool: "slack_post"}, Required: true,
		SourcePageID: "chat-conventions-page",
	}); err != nil {
		t.Fatalf("PutPage: %v", err)
	}
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: &scriptedProvider{}}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	reg := tools.NewRegistry()
	if err := reg.Register(stubTool{name: "slack_post", out: "posted"}, tools.OriginBuiltin); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if _, err := builtin.Register(reg, builtin.Deps{ToolSkills: registry,
		Authorize: builtin.Decide(authz.NoChart{})}); err != nil {
		t.Fatalf("builtin.Register: %v", err)
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	organization := &org.Organization{Name: "Acme", Roles: []*org.Role{role}}
	launcher := &probingLauncher{basis: basis}
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: organization, Role: role},
		Registry: reg, Models: models, Skills: registry,
		Launcher: launcher, Task: "post the summary",
		Resume: &runner.Resume{State: execstate.State{Version: execstate.Version, Round: 1}},
		Turn: runner.Turn{RunID: "t-guard", WorkKey: "wk-t-guard", AgentID: "agent-1",
			Context: &turnctx.Turn{RunID: "t-guard", WorkKey: "wk-t-guard", Seat: role, Org: organization}},
		Sessions: sessions,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	history := []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{
		{Name: builtin.LoadToolSkillTool, Args: map[string]any{"key": "chat-conventions"}, Result: "loaded"},
	}}}
	if _, _, err := r.Execute(t.Context(), 2, "thread the reply", history); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return launcher.last(t)
}

// A RELAUNCH THAT CONTINUES THE SESSION KEEPS WHAT IT LOADED; ONE THAT STARTS
// FRESH DOES NOT. The continued model has the skill body its earlier round
// loaded in front of it, so its guard holds that load and the covered tool
// runs; a fresh session read no skill, so its guard is empty and the tool is
// refused until it loads one. Within the turn: a later turn's first launch
// continues nothing yet.
//
// Mutation: rebuild a fresh relaunch's guard from the turn's loads, or a
// continued one's empty, and a row goes red.
func TestAContinuedRelaunchKeepsWhatItsSessionLoaded(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		basis   runner.SessionBasis
		refused bool
	}{
		"continued": {basis: keptBasis},
		"fresh":     {basis: runner.SessionBasis{Why: "the turn has kept no capture of its session"}, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := guardedRelaunch(t, tc.basis)
			bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})
			if bridged[0].Failed != tc.refused {
				t.Fatalf("slack_post failed %v (%q), want refused %v", bridged[0].Failed, bridged[0].Result,
					tc.refused)
			}
			if (req.Session != nil) != !tc.refused {
				t.Errorf("the relaunch continues %+v", req.Session)
			}
		})
	}
}

// A SESSION TOO LARGE TO CARRY IS NOT CONTINUED IN ITS BOX EITHER. A person's
// answer carries a parked run on in the session its box holds where the turn
// kept no session it can continue — but where the turn kept one whose context
// is past what a relaunch carries, the box's copy is that same session, as
// large or larger, so the run starts a session of its own instead. A session
// the turn did not keep is no reason to give up the box's own.
//
// Mutation: hand the launch the asking run's session whatever its launcher said
// of the kept one's size, or withhold it whenever no kept session continues, and
// a row goes red.
func TestASessionTooLargeToCarryIsNotContinuedInItsBoxEither(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	before := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"looking into it"}`)}})
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	for _, tc := range []struct {
		name    string
		basis   runner.SessionBasis
		inPlace string
	}{
		{name: "too large to carry", basis: runner.SessionBasis{Why: "past the window", Oversized: true}},
		{name: "none kept", basis: runner.SessionBasis{Why: "the turn has kept no capture"}, inPlace: "sess-ask"},
	} {
		relauncher := &probingLauncher{basis: tc.basis}
		asked := &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder",
			Branch: "wip/t1", SessionID: "sess-ask", BoxKept: true}
		collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
			resume: &runner.Resume{State: parked.State, Asked: asked, Bridged: before}})
		if _, _, err := collector.Resume(t.Context(), nil); err != nil {
			t.Fatalf("%s: Resume: %v", tc.name, err)
		}
		again := relauncher.last(t)
		if again.Session != nil || again.Continue == nil || again.Continue.SessionID != tc.inPlace {
			t.Errorf("%s: the relaunch continues kept %+v and in place %+v, want in place %q",
				tc.name, again.Session, again.Continue, tc.inPlace)
		}
	}
}
