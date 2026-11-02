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
	// probes counts the times it was asked.
	probes int
}

func (l *probingLauncher) ProbeSession(context.Context) runner.SessionBasis {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.probes++
	return l.basis
}

// asked is how many times the launcher was asked which session a relaunch
// continues.
func (l *probingLauncher) asked() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.probes
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
// the turn has read that the session has not — the one read after the session
// was saved, and the one the reviewer read in this very segment, which reaches
// the brief before the record has numbered it — then the one this brief is the
// first to read, never one the session read itself. In a box that replaced the
// session's, it is told that too: a fresh checkout, and the old box's paths. The
// fresh brief beside it still carries every note.
//
// Mutation: carry only the notes this brief is the first to read, or every
// note the turn read, drop a note this segment read, or drop the task or the
// rules, and this goes red.
func TestASentBackSessionIsToldWhatItLacks(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"first pass"}`)}})
	parked, _ := launched.Suspended()

	read := steer.Note{ID: "read", Text: "use the staging database", By: "founder"}
	reviewed := steer.Note{ID: "reviewed", Text: "and keep the old flag", By: "founder"}
	reviewedNow := steer.Note{ID: "reviewed-now", Text: "and mind the migration", By: "founder"}
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
				// READ BY THIS SEGMENT'S REVIEW, which no record has
				// numbered yet: a review drains its notes before the
				// send-back it decides on.
				{Note: reviewedNow, Reading: steer.Reading{Phase: types.PhaseReview, Iteration: 1, Round: 1,
					Boundary: types.SteerBoundaryRound}},
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
		prompts.SteerMessage(reviewedNow.By, reviewedNow.Text), prompts.SteerMessage(fresh.By, fresh.Text)} {
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
	for _, note := range []steer.Note{read, reviewed, reviewedNow, fresh} {
		if !strings.Contains(again.Brief, prompts.SteerMessage(note.By, note.Text)) {
			t.Errorf("the fresh brief does not carry note %q", note.ID)
		}
	}
	if relauncher.asked() != 1 {
		t.Errorf("the launcher was asked %d times which session the send-back continues, want once",
			relauncher.asked())
	}
}

// A CARRIED-ON SESSION IS TOLD WHAT CAME AFTER IT WAS SAVED. A lost job's round
// continues the job's kept session, saved before the job was cut off: so it is
// told it was cut off, that its transcript may end mid-call, and every call the
// run made through the seat's tools that the session had not been answered —
// after the point it ends, and one still in flight when it was saved — with
// the call's outcome, never one the session holds. A round lost a second time
// starts a session of its own, rather than replay the state it was lost in
// toward the round's limit.
//
// Mutation: list every call of the round, leave out a call in flight at the
// save, drop the interrupted note, or continue the session after a second
// loss, and a case goes red.
func TestACarriedOnSessionIsToldWhatCameAfterItWasSaved(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	bridged := play(t, req, run{calls: []llm.ToolCall{
		call(t, "slack_post", `{"text":"starting on it"}`),
		call(t, "jira_create", `{"summary":"after the save"}`),
		call(t, "slack_history", `{"channel":"C1"}`),
	}})
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()

	carriedOn := func(basis runner.SessionBasis, losses int) runner.AgentRunRequest {
		t.Helper()
		relauncher := &probingLauncher{basis: basis}
		lost := runner.Lost{Why: "the engine it ran under restarted, which killed it", BoxKept: true,
			Losses: losses, Limit: 2}
		collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
			resume: &runner.Resume{State: parked.State, Bridged: bridged, Lost: &lost}})
		if _, _, err := collector.Resume(t.Context(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		if losses <= 1 && relauncher.asked() != 1 {
			t.Errorf("the launcher was asked %d times, want once", relauncher.asked())
		}
		return relauncher.last(t)
	}

	// SAVED ONCE ITS FIRST CALL WAS ANSWERED: the two after it are not in
	// the session.
	basis := keptBasis
	basis.Interrupted, basis.BridgeSeq, basis.BridgeOpen = true, 1, nil
	first := carriedOn(basis, 1)
	if first.Session == nil {
		t.Fatal("a lost job's round continues no kept session")
	}
	brief := first.Session.Brief
	for _, want := range []string{"## Your previous segment was cut off", prompts.InterruptedSession,
		"## Calls made after the session above was saved", "jira_create", "slack_history"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the carried-on session is not told %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "slack_post") {
		t.Errorf("the carried-on session is handed a call it holds:\n%s", brief)
	}

	// SAVED ONCE ITS SECOND CALL WAS ANSWERED, while its first — a slow one,
	// numbered before it — was still in flight: the first's outcome is not in
	// the session either.
	inFlight := keptBasis
	inFlight.Interrupted, inFlight.BridgeSeq, inFlight.BridgeOpen = true, 2, []int{1}
	if brief := carriedOn(inFlight, 1).Session.Brief; !strings.Contains(brief, "slack_post") ||
		!strings.Contains(brief, "slack_history") || strings.Contains(brief, "jira_create") {
		t.Errorf("saved with its first call in flight, the session is told:\n%s\nwant the call in flight "+
			"and the one after the save, never the one it holds", brief)
	}

	if second := carriedOn(basis, 2); second.Session != nil {
		t.Errorf("a round lost a second time continues %+v, want a session of its own", second.Session)
	}
}

// AN ANSWERED OR A REFUSED RUN'S SESSION IS CONTINUED, TOLD WHAT CARRIES IT ON.
// A person's answer carries the asking run on in the session that asked, and a
// replaced sign-in the refused run's: the job each follows left its session
// kept, so the relaunch continues it in whatever box it gets — told the
// answer, or why its last segment ended, and in a box that replaced its own,
// the branch its work is on — asked once of the launcher, as a relaunch that
// carries its round on.
//
// Mutation: carry either round on in a fresh session where its job's session
// was kept, or tell it the wrong thing, and its case goes red.
func TestAnAnsweredOrARefusedRunsSessionIsContinuedToldWhatCarriesItOn(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	before := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"looking into it"}`)}})
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	for name, tc := range map[string]struct {
		resume func(*runner.Resume)
		want   []string
		branch string
	}{
		"answered": {
			resume: func(r *runner.Resume) {
				r.Asked = &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder",
					Branch: "wip/t1", SessionID: "s-1", BoxKept: true}
			},
			want:   []string{"## Your question was answered", "use main"},
			branch: "wip/t1",
		},
		"refused on a replaced sign-in": {
			resume: func(r *runner.Resume) {
				r.Rotated = &runner.Rotated{SessionID: "s-1", BoxKept: true}
				r.Answer = "The executor's run did NOT fully succeed.\n\nError: Failed to authenticate"
			},
			want: []string{runner.RotatedContinuation},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			relauncher := &probingLauncher{basis: keptBasis}
			resume := &runner.Resume{State: parked.State, Bridged: before}
			tc.resume(resume)
			collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
				resume: resume})
			if _, _, err := collector.Resume(t.Context(), nil); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			again := relauncher.last(t)
			if again.Session == nil || again.Session.Basis.SessionID != "s-1" || relauncher.asked() != 1 {
				t.Fatalf("the relaunch continues %+v (asked %d times), want the kept session of the job "+
					"it follows, asked once", again.Session, relauncher.asked())
			}
			for _, want := range tc.want {
				if !strings.Contains(again.Session.Brief, want) {
					t.Errorf("the continued session is not told %q:\n%s", want, again.Session.Brief)
				}
			}
			replaced := again.Session.Replaced
			if !strings.Contains(replaced, "## This run is in a new box") ||
				(tc.branch != "" && !strings.Contains(replaced, "`"+tc.branch+"`")) {
				t.Errorf("the new-box continuation does not say what a replaced box changes:\n%s", replaced)
			}
		})
	}
}

// A RELAUNCH HANDS ITS LAUNCH WHAT ITS SESSION SENDS FIRST, kept or not. A
// person's answer carries the asking run on in the session its box holds where
// the launch takes up no kept capture of it — whose newest capture was
// refused, say — and that session's whole context is what the run sends before
// the answer: the launch is handed it, as the newest capture measured it, to
// hold the launch to the floor of what it sends ([runner.Continuation]), where
// whether the seat's budget can carry it at all is the launch's to decide.
//
// Mutation: hand the launch the context only of a kept capture it continues, or
// withhold the box's own session for its size here, and this goes red.
func TestARelaunchHandsItsLaunchWhatItsSessionSendsFirst(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	before := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"looking into it"}`)}})
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	relauncher := &probingLauncher{basis: runner.SessionBasis{
		Why: "the newest capture of the job was not kept: refused", ContextTokens: 150000}}
	asked := &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder",
		Branch: "wip/t1", SessionID: "sess-ask", BoxKept: true}
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
		resume: &runner.Resume{State: parked.State, Asked: asked, Bridged: before}})
	if _, _, err := collector.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again := relauncher.last(t)
	if again.Session != nil || again.Continue == nil || again.Continue.SessionID != "sess-ask" ||
		again.Continue.ContextTokens != 150000 {
		t.Fatalf("the relaunch continues kept %+v and in place %+v, want the asking run's own session "+
			"in place, carrying the 150000 tokens it sends first", again.Session, again.Continue)
	}
}

// guarded is a seat whose surface carries one required skill covering
// slack_post, relaunching its executor in round 2 of its turn — with what its
// launcher says the relaunch continues, the job before's parked state and
// calls, whether that job's box said it took up the session its launch chose
// (its record's word, [runner.Resume.Continued]), the turn's closed rounds, how
// the launch decides, and what the relaunched job's own box says it started.
type guarded struct {
	basis     runner.SessionBasis
	state     execstate.State
	bridged   []runner.BridgedCall
	continued bool
	history   []ledger.Iteration
	decides   func(runner.AgentRunRequest) runner.SessionDecision
	says      *bool
}

// loadSkill is a call that loaded the seat's one required skill.
func loadSkill(seq int) runner.BridgedCall {
	return runner.BridgedCall{Call: ledger.Call{Name: builtin.LoadToolSkillTool,
		Args: map[string]any{"key": "chat-conventions"}, Result: "Always thread your reply."}, Seq: seq}
}

// relaunch builds g's seat and relaunches its executor, returning what the
// launch was handed.
func (g guarded) relaunch(t *testing.T) runner.AgentRunRequest {
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
	launcher := &probingLauncher{basis: g.basis}
	launcher.decides, launcher.says = g.decides, g.says
	state := g.state
	state.Version, state.Round = execstate.Version, 1
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: organization, Role: role},
		Registry: reg, Models: models, Skills: registry,
		Launcher: launcher, Task: "post the summary",
		Resume: &runner.Resume{State: state, Bridged: g.bridged, Continued: g.continued},
		Turn: runner.Turn{RunID: "t-guard", WorkKey: "wk-t-guard", AgentID: "agent-1",
			Context: &turnctx.Turn{RunID: "t-guard", WorkKey: "wk-t-guard", Seat: role, Org: organization}},
		Sessions: sessions,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	if _, _, err := r.Execute(t.Context(), 2, "thread the reply", g.history); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return launcher.last(t)
}

// A RELAUNCH'S GUARD HOLDS WHAT THE SESSION ITS RUN TAKES UP LOADED, AND NOTHING
// ELSE. The continued model has in front of it the skill bodies its own session
// loaded — those its launch was armed with, and those its job loaded up to the
// point its capture was saved — so its guard holds those and the covered tool
// runs. Not a load another session made: an earlier round's, in a session this
// one never was, or one the job made after the capture, which the restored
// session never saw. Not the skills the job before's launch was armed with
// where that job's box said it started a session of its own in place of the one
// its launch chose (its record's word): the session the relaunch continues is
// that one, which read none of them. And a session of the run's own read no
// skill, whether the relaunch meant to start one or its launch found it could
// not continue the session after all: its guard is empty, and the tool is
// refused until it loads one. Armed as the launch decides, before the run makes
// a call.
//
// Mutation: rebuild a continued relaunch's guard from the turn's loads, take
// the job before's armed skills whatever its box said, arm it before the launch
// decides, or keep a fresh one's from the session it meant to continue, and a
// row goes red.
func TestARelaunchsGuardHoldsWhatItsSessionLoaded(t *testing.T) {
	t.Parallel()
	earlierRound := []ledger.Iteration{{Iteration: 1, Calls: []ledger.Call{loadSkill(1).Call}}}
	goesFresh := func(runner.AgentRunRequest) runner.SessionDecision { return runner.SessionDecision{} }
	for name, tc := range map[string]struct {
		g       guarded
		refused bool
	}{
		"its launch armed it with the load": {g: guarded{basis: keptBasis, continued: true,
			state: execstate.State{LoadedSkills: []string{"chat-conventions"}}}},
		"its launch armed it, and its box started a session of its own": {g: guarded{basis: keptBasis,
			state: execstate.State{LoadedSkills: []string{"chat-conventions"}}}, refused: true},
		"its own job loaded it before the save": {g: guarded{basis: keptBasis,
			bridged: []runner.BridgedCall{loadSkill(1)}}},
		"an earlier round's session loaded it": {g: guarded{basis: keptBasis, history: earlierRound},
			refused: true},
		"its job loaded it after the save": {g: guarded{basis: keptBasis,
			bridged: []runner.BridgedCall{loadSkill(2)}}, refused: true},
		"an earlier job of the round loaded it": {g: guarded{basis: keptBasis,
			state: execstate.State{BridgeFrom: 1}, bridged: []runner.BridgedCall{loadSkill(1)}}, refused: true},
		"a fresh session": {g: guarded{basis: runner.SessionBasis{Why: "the turn has kept no capture"},
			state: execstate.State{LoadedSkills: []string{"chat-conventions"}}}, refused: true},
		"a launch that went fresh after all": {g: guarded{basis: keptBasis,
			state: execstate.State{LoadedSkills: []string{"chat-conventions"}}, decides: goesFresh}, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := tc.g.relaunch(t)
			bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})
			if bridged[0].Failed != tc.refused {
				t.Fatalf("slack_post failed %v (%q), want refused %v", bridged[0].Failed, bridged[0].Result,
					tc.refused)
			}
		})
	}
}

// A RUN WHOSE BOX STARTS A SESSION OF ITS OWN IS HELD TO A FRESH GUARD BEFORE
// ITS FIRST CALL. The launch decided to continue the session of the job before,
// which loaded the skill, and armed the run's guard with it; the run's box then
// found it could not take that session up — an archive that did not unpack, an
// import OpenCode refused — and started one of its own, which it says by the
// token its harness dials before it makes a call. That session's model has read
// no skill, so the call is refused until it loads one, and the phase's record
// states the fresh brief its run was handed in the continuation's place. A box
// that says it took the session up keeps the guard its launch armed.
//
// Mutation: ignore the box's word, or re-arm on it whichever session it names,
// and a case goes red; publish nothing on a fall back, and the record says the
// run read a continuation it never did.
func TestARunWhoseBoxStartsASessionOfItsOwnIsHeldToAFreshGuard(t *testing.T) {
	t.Parallel()
	fellBack, tookItUp := false, true
	for name, tc := range map[string]struct {
		says    *bool
		refused bool
	}{
		"its box started a session of its own": {says: &fellBack, refused: true},
		"its box took the session up":          {says: &tookItUp},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := guarded{basis: keptBasis, continued: true, says: tc.says,
				state: execstate.State{LoadedSkills: []string{"chat-conventions"}}}.relaunch(t)
			bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})
			if bridged[0].Failed != tc.refused {
				t.Fatalf("slack_post failed %v (%q), want refused %v", bridged[0].Failed, bridged[0].Result,
					tc.refused)
			}
		})
	}
}

// A RUN THAT FELL BACK IS RECORDED READING THE FRESH BRIEF. Its launch published
// the continuation it handed the run as the round's opening frame; the run's box
// then started a session of its own on the fresh brief instead, and says so
// before the run's first call — so the round's record then states the fresh
// brief, the prompt the run did read. A box that took the session up publishes
// nothing more.
//
// Mutation: publish nothing on the box's word, or publish on either word, and a
// case goes red.
func TestARunThatFellBackIsRecordedReadingTheFreshBrief(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"first pass"}`)}})
	parked, _ := launched.Suspended()
	fellBack, tookItUp := false, true
	for name, tc := range map[string]struct {
		says      *bool
		published int
	}{
		"its box started a session of its own": {says: &fellBack, published: 3},
		"its box took the session up":          {says: &tookItUp, published: 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pub := newCapture()
			relauncher := &probingLauncher{basis: keptBasis}
			relauncher.says = tc.says
			collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
				pub: pub, resume: &runner.Resume{State: parked.State, Bridged: bridged, Answer: "first pass done",
					Continued: true}})
			if _, _, err := collector.Resume(t.Context(), nil); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			history := []ledger.Iteration{{Iteration: 1, Calls: ledgerOf(bridged)}}
			if _, _, err := collector.Execute(t.Context(), 2, "the test still fails on CI", history); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			again := relauncher.last(t)
			published := pub.promptsFor("execute")
			if len(published) != tc.published {
				t.Fatalf("the relaunch published %d prompts, want %d", len(published), tc.published)
			}
			if last := published[len(published)-1]; tc.published == 3 && !strings.Contains(last, again.Brief) {
				t.Errorf("the round's last frame states %q, want the fresh brief its run read", last)
			}
		})
	}
}

// A LAUNCH PUBLISHES THE PROMPT ITS RUN WAS HANDED. The executor's started record
// carries the fresh brief, published before the launch knows its box; a
// relaunch that continues its session is handed a continuation instead — the
// one for a box that replaced the session's, where it got one — and that is
// what the phase's opening frame then states, before the run starts. A launch
// that hands the fresh brief publishes nothing more.
//
// Mutation: publish the fresh brief alone, or the same-box continuation to a
// run in a new box, and this goes red.
func TestALaunchPublishesThePromptItsRunWasHanded(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"first pass"}`)}})
	parked, _ := launched.Suspended()
	for name, basis := range map[string]runner.SessionBasis{
		"continued in a new box": keptBasis,
		"fresh":                  {Why: "the turn has kept no capture"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pub := newCapture()
			relauncher := &probingLauncher{basis: basis}
			collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
				pub: pub, resume: &runner.Resume{State: parked.State, Bridged: bridged, Answer: "first pass done"}})
			if _, _, err := collector.Resume(t.Context(), nil); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			history := []ledger.Iteration{{Iteration: 1, Calls: ledgerOf(bridged)}}
			if _, _, err := collector.Execute(t.Context(), 2, "the test still fails on CI", history); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			again := relauncher.last(t)
			published := pub.promptsFor("execute")
			if again.Session == nil {
				if len(published) != 1 || !strings.Contains(published[0], again.Brief) {
					t.Fatalf("a fresh relaunch published %d prompts, want its started record's fresh brief alone",
						len(published))
				}
				return
			}
			if len(published) != 2 || !strings.Contains(published[0], again.Brief) ||
				!strings.Contains(published[1], again.Session.Replaced) {
				t.Fatalf("a continued relaunch published %d prompts, want the fresh brief and then the "+
					"new-box continuation its run was handed", len(published))
			}
		})
	}
}
