package engine

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// testLeads is the seal a coordinator a case builds by hand seals the leads its
// launches' briefs open with under ([sandbox.CoordinatorOptions.Leads]), and a
// resume opens them with ([runnerResume]): the engine cases' one fleet keyring,
// as a fleet shares one.
var testLeads = sync.OnceValue(func() *sandbox.LeadSeal {
	seal, err := sandbox.NewLeadSeal(recordseal.OneKey("answers", "the engine cases' fleet key material"))
	if err != nil {
		panic(err)
	}
	return seal
})

// leadsHeading opens the leads a session is told again.
const leadsHeading = "## Messages that carried this work on that the session above may not show"

// relaunchHanded is the request a relaunch built from resume hands its launcher:
// the runner c builds for its seat handle's turn tc, as a resumed segment's is,
// relaunching through a launcher that keeps what it is handed and launches
// nothing ([relaunchRecorder]).
func relaunchHanded(t *testing.T, c *Company, handle string, tc *turnctx.Turn,
	resume *runner.Resume) *runner.AgentRunRequest {

	t.Helper()
	recorder := &relaunchRecorder{}
	r, err := c.RunnerFor(t.Context(), handle, tools.NewRegistry(), RunnerInput{
		Task: "fix the failing test", Reply: turn.ToolReply(""), Launcher: recorder,
		Sessions: testSessions(t), Resume: resume,
		Turn: runner.Turn{RunID: tc.RunID, ConversationKey: tc.ConversationKey, Context: tc},
	})
	if err != nil {
		t.Fatalf("RunnerFor: %v", err)
	}
	// REFUSED BY THE RECORDER, which launches nothing: what it was handed is
	// the case's.
	_, _, _ = r.Resume(t.Context(), nil)
	handed := recorder.handed()
	if handed == nil || handed.Continue == nil || handed.Continue.SessionID == "" {
		t.Fatalf("the relaunch continues no copy in its job's box: %+v", handed)
	}
	return handed
}

// A RESUME OPENS THE LEADS ITS JOB'S BRIEF TOLD. The run keeps the turn's leads
// beside its record, oldest first, each sealed by the job whose brief first told
// it and numbered as taken, and the job's record marks the ones its own brief
// told by those numbers: the resume hands the runner every lead it read beside
// the row opened, in order, those the record marks as told (a held call's
// result opened from the seal its row put on it), with how many earlier ones
// are not kept and where the turn began. A lead under a key
// this node does not hold is handed on as one that cannot be read here, by its
// kind, job and round: never dropped, so the brief that tells it again says a
// message is missing. A held call's result that does not open here, under a key
// this node lacks or with no call seal at all, leaves its lead told without it,
// as a call made whose result cannot be read here.
//
// Mutation: drop a lead that does not open, read every lead as told, or none,
// open no held call's result, or leave the count or the turn's base off the
// resume, and a check goes red.
func TestAResumeOpensTheLeadsItsJobsBriefTold(t *testing.T) {
	t.Parallel()
	seal := func(s *sandbox.LeadSeal, lead sandbox.Lead, words string) sandbox.KeptLead {
		t.Helper()
		lead.Kind, lead.Round = sandbox.LeadKindAnswer, 1
		if lead.Answer == nil {
			lead.Answer = &sandbox.LeadAnswer{Question: "which branch?", Words: words, By: "founder"}
		}
		kept, err := s.Seal("t1", lead)
		if err != nil {
			t.Fatalf("Seal: %v", err)
		}
		return kept
	}
	newer, err := sandbox.NewLeadSeal(recordseal.OneKey("leads-k2", "the key a newer node seals under"))
	if err != nil {
		t.Fatal(err)
	}
	// A HELD CALL'S LEAD, its result as the run's row sealed it.
	calls := sandbox.NewCallSeal(valueRing(t, "k1"))
	call := sandbox.HeldCall{ID: "held-1", LaunchID: "job-4", Server: "vendor", Tool: "vendor_post", Raw: "post",
		Digest: "sha256:d1"}
	posted, err := calls.SealResult("t1", call, sandbox.HeldResult{Output: "posted"})
	if err != nil {
		t.Fatal(err)
	}
	kept := sandbox.KeptAs(call, posted)
	held := sandbox.Lead{Job: "job-4", Seq: 4, Answer: &sandbox.LeadAnswer{Question: "May swe call vendor_post?",
		By: "founder", Held: &sandbox.LeadHeld{Tool: "vendor_post", Server: "vendor", HeldID: "held-1",
			Choice: sandbox.HeldOnce, Outcome: sandbox.LeadHeldMade, Result: &kept}}}
	// THE FOUR LEADS AS THE RUN KEEPS THEM, numbered as taken, the first an
	// earlier job's the record does not mark.
	part := []sandbox.KeptLead{seal(testLeads(), sandbox.Lead{Job: "job-1", Seq: 1}, "an earlier answer"),
		seal(testLeads(), sandbox.Lead{Job: "job-2", Seq: 2}, "use main"),
		seal(newer, sandbox.Lead{Job: "job-3", Seq: 3}, "unread"), seal(testLeads(), held, "")}
	for i := range part {
		part[i].Appended = i + 1
	}
	read := sandbox.TurnLeads{Kept: part, Cut: 2}
	run := sandbox.PendingRun{TurnID: "t1",
		Base: &sandbox.ConversationBase{Session: &sandbox.BaseSession{Turn: "t-before", SessionID: "s-before"}},
		Launch: sandbox.LaunchRecord{Brief: sandbox.BriefContinued, SessionStart: sandbox.SessionStartContinued,
			LeadMarks: []int{2, 3, 4}}}
	got := runnerResume(resumeInput{Run: run, Leads: read}, nil, calls, testLeads())
	if len(got.Leads) != 4 {
		t.Fatalf("the resume hands on %d leads, want the 4 it read: %+v", len(got.Leads), got.Leads)
	}
	if first := got.Leads[0]; first.Told || first.Lead.Answer == nil || first.Lead.Answer.Words != "an earlier answer" {
		t.Errorf("the earlier lead is handed on as %+v, want it opened and not told by this job's brief", first)
	}
	if second := got.Leads[1]; !second.Told || second.Lead.Answer == nil || second.Lead.Answer.Words != "use main" {
		t.Errorf("the job's own lead is handed on as %+v, want it opened and told", second)
	}
	if third := got.Leads[2]; !third.Told || third.Lead.Unread == "" || third.Lead.Job != "job-3" ||
		third.Lead.Kind != sandbox.LeadKindAnswer || third.Lead.Answer != nil {
		t.Errorf("the lead under a key this node lacks is handed on as %+v, want it told, unread, by its kind "+
			"and job alone", third)
	}
	if got.LeadsCut != 2 || got.Base == nil || got.Base.Turn != "t-before" {
		t.Errorf("the resume hands on %d leads cut and base %+v, want 2 and the turn's base, t-before",
			got.LeadsCut, got.Base)
	}
	if fourth := got.Leads[3]; !fourth.Told || fourth.Result == nil || fourth.Result.Output != "posted" {
		t.Errorf("the held call's lead is handed on as %+v, result %+v; want it told with what the call returned, "+
			"opened from the seal its row put on it", fourth, fourth.Result)
	}
	for name, elsewhere := range map[string]*sandbox.CallSeal{
		"under a key this node lacks": sandbox.NewCallSeal(valueRing(t, "k2")),
		"with no call seal":           nil,
	} {
		fourth := runnerResume(resumeInput{Run: run, Leads: read}, nil, elsewhere, testLeads()).Leads[3]
		if !fourth.Told || fourth.Result != nil || fourth.Lead.Answer == nil || fourth.Lead.Answer.Held == nil ||
			fourth.Lead.Answer.Held.Outcome != sandbox.LeadHeldMade {
			t.Errorf("%s, the held call's lead is handed on as %+v; want it told, made, without a result it "+
				"cannot open", name, fourth)
		}
	}
}

// A RESUME OF A JOB WHOSE SESSION HOLDS ITS BRIEF TELLS NO LEAD OF IT AGAIN. A
// job whose launch handed its run a fresh brief, or whose box started a session
// of its own in a continued one's place, holds that brief wherever its session
// is continued, the lead it opened with among it, whatever the record marked
// told when the brief was handed: the resume hands its leads on as ones the
// job's brief did not tell (sandbox.LaunchRecord.ToldLeads), so no continuation
// of that session tells them again.
//
// Mutation: read the record's marks whatever the session its job's box
// started, and a case goes red.
func TestAResumeOfAJobWhoseSessionHoldsItsBriefTellsNoLeadOfItAgain(t *testing.T) {
	t.Parallel()
	own, err := testLeads().Seal("t1", sandbox.Lead{Kind: sandbox.LeadKindAnswer, Job: "job-1", Seq: 1, Round: 1,
		Answer: &sandbox.LeadAnswer{Question: "which branch?", Words: "use main", By: "founder"}})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	own.Appended = 1
	read := sandbox.TurnLeads{Kept: []sandbox.KeptLead{own}}
	for name, launch := range map[string]sandbox.LaunchRecord{
		"its brief fresh": {Brief: sandbox.BriefFresh, LeadMarks: []int{1}},
		"its box having started a session of its own": {Brief: sandbox.BriefContinued,
			SessionStart: sandbox.SessionStartFresh, LeadMarks: []int{1}},
	} {
		got := runnerResume(resumeInput{Run: sandbox.PendingRun{TurnID: "t1", Launch: launch}, Leads: read}, nil,
			nil, testLeads())
		if len(got.Leads) != 1 || got.Leads[0].Told || got.Leads[0].Lead.Answer == nil {
			t.Errorf("%s, the resume hands on %+v; want the job's lead opened and not told by its brief", name,
				got.Leads)
		}
	}
}

// A RESUME THAT COULD NOT READ ITS LEADS HANDS ON THE ONES ITS JOB'S BRIEF TOLD.
// Where the coordinator could not read the run's leads at all, the resume knows
// the ones the job's brief told by their numbers alone, off the job's record:
// it hands the runner no lead, every one the row counts as not kept, and those
// numbers, for a continuation of the job's session to hand on, so the job its
// launch opens marks them and a later read tells them. Where the leads were
// read it hands none on by number, since the ones the brief told ride the
// leads, marked; and a job whose session holds its brief hands none on either
// way (sandbox.LaunchRecord.ToldLeads).
//
// Mutation: hand the numbers on whatever the read, never, or off the record's
// marks whatever its session, and a case goes red.
func TestAResumeThatCouldNotReadItsLeadsHandsOnTheOnesItsJobsBriefTold(t *testing.T) {
	t.Parallel()
	own, err := testLeads().Seal("t1", sandbox.Lead{Kind: sandbox.LeadKindAnswer, Job: "job-2", Seq: 2, Round: 1,
		Answer: &sandbox.LeadAnswer{Question: "which branch?", Words: "use main", By: "founder"}})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	own.Appended = 2
	continued := sandbox.LaunchRecord{Brief: sandbox.BriefContinued, SessionStart: sandbox.SessionStartContinued,
		LeadMarks: []int{2, 4}}
	unread := sandbox.TurnLeads{Cut: 4, Unread: true}
	for name, tc := range map[string]struct {
		launch sandbox.LaunchRecord
		read   sandbox.TurnLeads
		want   []int
		leads  int
	}{
		"the leads unread": {launch: continued, read: unread, want: []int{2, 4}},
		"the leads read":   {launch: continued, read: sandbox.TurnLeads{Kept: []sandbox.KeptLead{own}, Cut: 3}, leads: 1},
		"the leads unread, its brief fresh": {launch: sandbox.LaunchRecord{Brief: sandbox.BriefFresh,
			LeadMarks: []int{2, 4}}, read: unread},
	} {
		got := runnerResume(resumeInput{Run: sandbox.PendingRun{TurnID: "t1", Launch: tc.launch}, Leads: tc.read},
			nil, nil, testLeads())
		if !slices.Equal(got.LeadsUnread, tc.want) || len(got.Leads) != tc.leads || got.LeadsCut != tc.read.Cut {
			t.Errorf("%s: the resume hands on %v unread beside %d leads and %d cut; want %v, %d and %d", name,
				got.LeadsUnread, len(got.Leads), got.LeadsCut, tc.want, tc.leads, tc.read.Cut)
		}
	}
}

// AN ANSWER CARRIED ON IN A CUT-OFF BOX REACHES THE NEXT BRIEF AGAIN, end to end,
// through the real launcher and the coordination store: a person's answer
// carries the round on in the asking job's own box, the runner building the
// relaunch and the launch keeping the lead its brief opened with beside the
// run's record, marked on the job's; that job is refused on a replaced sign-in
// before its harness wrote the brief, so the copy in its box may lack the
// answer, and the relaunch the replaced sign-in drives, read the run's leads,
// tells it the answer again.
//
// Mutation: keep no lead at the launch, or read none at the resume, and the
// second relaunch is told nothing of the answer.
func TestAnAnswerCarriedOnInACutOffBoxReachesTheNextBriefAgain(t *testing.T) {
	t.Parallel()
	const turnID = "t-told-again"
	asking := launchBridged(t, turnID)
	// THE ASKING JOB PARKED, ITS ANSWER CLAIMED: the relaunch carrying the round
	// on finds its box kept and continues the session there.
	parked := asking.row(t, turnID)
	parkedOnAHeldCall(t, asking.pending, turnID, parked.LaunchID)
	if _, ok, err := asking.pending.ClaimForResume(t.Context(), turnID,
		sandbox.RecordedAnswerTail(parked.LaunchID), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	state := execstate.State{Version: execstate.Version, Round: 1}
	handed := relaunchHanded(t, asking.launcher.company, asking.handle, asking.launcher.turn,
		runnerResume(resumeInput{Run: asking.row(t, turnID), State: state,
			Asked: &sandbox.AskedRun{Question: "which branch?", Answer: "use the release branch", By: "founder",
				SessionID: "sess-ask", BoxKept: true}}, nil, nil, testLeads()))
	carried := asking.launch(t, handed.Continue)
	job := carried.row(t, turnID)
	leads, err := carried.pending.Leads(t.Context(), job)
	if err != nil {
		t.Fatal(err)
	}
	if job.Launch.Brief != sandbox.BriefContinued || len(leads.Kept) != 1 || leads.Kept[0].Job != job.LaunchID ||
		!slices.Equal(job.Launch.LeadMarks, []int{leads.Kept[0].Appended}) {
		t.Fatalf("the answer's job records brief %q and marks %v beside leads %+v; want the copy continued in its "+
			"box and the answer it opened with, sealed for job %s and marked", job.Launch.Brief,
			job.Launch.LeadMarks, leads.Kept, job.LaunchID)
	}

	// REFUSED ON A REPLACED SIGN-IN BEFORE ITS HARNESS WROTE THE BRIEF.
	job.Launch.HarnessEnd = sandbox.HarnessEndCutOff
	brief := relaunchHanded(t, carried.launcher.company, carried.handle, carried.launcher.turn,
		runnerResume(resumeInput{Run: job, State: state, Leads: leads,
			Rotated: &runner.Rotated{SessionID: "sess-ask", BoxKept: true}}, nil, nil, testLeads())).Continue.Brief
	cut, told := strings.Index(brief, "## The session above was cut off"), strings.Index(brief, leadsHeading)
	if !strings.HasPrefix(brief, runner.RotatedContinuation) || cut < 0 || told < cut ||
		!strings.Contains(brief[told:], "founder answered: use the release branch") {
		t.Errorf("the replaced sign-in's relaunch is not told the answer its copy may lack, after what carries "+
			"it on and the cut-off section:\n%s", brief)
	}
}

// A CHAIN OF CUT-OFF SESSIONS OUTLIVES A READ OF ITS LEADS THAT FAILED, end to
// end, through the real launcher and the coordination store. A person's answer
// carries the round on in the asking job's own box, whose job marks it. That
// job, its harness cut off, asks in turn and is answered, and the resume that
// carries it on cannot read the run's leads: the copy it continues, which may
// lack the first brief, is told the first answer is not kept, and the job it
// opens marks that answer all the same, beside its own. Cut off in turn, that
// job's copy, the first copy carried on and so one that may lack both briefs,
// is told both answers once the read answers. Marked by the leads a read gave
// back alone, the first answer would leave the chain there: every later brief
// to that copy told neither it nor that it is missing.
//
// Mutation: drop the numbers at the resume, the relaunch or the launch, or
// leave them unmarked at the take, and the third brief is told nothing of the
// first answer.
func TestAChainOfCutOffSessionsOutlivesAReadOfItsLeadsThatFailed(t *testing.T) {
	t.Parallel()
	const turnID = "t-chain"
	ctx := t.Context()
	asking := launchBridged(t, turnID)
	parked := asking.row(t, turnID)
	parkedOnAHeldCall(t, asking.pending, turnID, parked.LaunchID)
	if _, ok, err := asking.pending.ClaimForResume(ctx, turnID,
		sandbox.RecordedAnswerTail(parked.LaunchID), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume = %v, %v", ok, err)
	}
	state := execstate.State{Version: execstate.Version, Round: 1}
	handed := relaunchHanded(t, asking.launcher.company, asking.handle, asking.launcher.turn,
		runnerResume(resumeInput{Run: asking.row(t, turnID), State: state,
			Asked: &sandbox.AskedRun{Question: "which branch?", Answer: "use the release branch", By: "founder",
				SessionID: "sess-ask", BoxKept: true}}, nil, nil, testLeads()))
	carried := asking.launch(t, handed.Continue)
	first := carried.row(t, turnID)
	if first.Launch.Brief != sandbox.BriefContinued || len(first.Launch.LeadMarks) != 1 {
		t.Fatalf("the first answer's job records brief %q and marks %v; want the copy continued in its box and "+
			"the answer it opened with marked", first.Launch.Brief, first.Launch.LeadMarks)
	}

	// CUT OFF, IT ASKS IN TURN AND IS ANSWERED, AND THE RUN'S LEADS CANNOT BE
	// READ: every one the row counts said as not kept (sandbox's leadsAt).
	parkedOnAHeldCall(t, carried.pending, turnID, first.LaunchID)
	if _, ok, err := carried.pending.ClaimForResume(ctx, turnID,
		sandbox.RecordedAnswerTail(first.LaunchID), sandbox.Fence{}); err != nil || !ok {
		t.Fatalf("ClaimForResume of the carried job = %v, %v", ok, err)
	}
	job := carried.row(t, turnID)
	job.Launch.HarnessEnd = sandbox.HarnessEndCutOff
	second := relaunchHanded(t, carried.launcher.company, carried.handle, carried.launcher.turn,
		runnerResume(resumeInput{Run: job, State: state,
			Leads: sandbox.TurnLeads{Cut: job.LeadsAppended, Unread: true},
			Asked: &sandbox.AskedRun{Question: "which tests?", Answer: "the unit tests", By: "founder",
				SessionID: "sess-ask", BoxKept: true}}, nil, nil, testLeads()))
	notKept := "(+1 earlier such message, which is not kept to tell again.)"
	if told := strings.Index(second.Continue.Brief, leadsHeading); told < 0 ||
		!strings.Contains(second.Continue.Brief[told:], notKept) ||
		strings.Contains(second.Continue.Brief, "use the release branch") {
		t.Fatalf("the copy whose resume could not read the leads is not told the first answer is not kept:\n%s",
			second.Continue.Brief)
	}
	again := carried.launch(t, second.Continue)
	middle := again.row(t, turnID)
	read, err := again.pending.Leads(ctx, middle)
	if err != nil || len(read.Kept) != 2 || middle.LaunchID == job.LaunchID ||
		middle.Launch.Brief != sandbox.BriefContinued {
		t.Fatalf("the second answer's job %s records brief %q beside leads %+v, %v; want a new job continuing "+
			"the copy, and both answers kept", middle.LaunchID, middle.Launch.Brief, read.Kept, err)
	}
	if want := []int{read.Kept[0].Appended, read.Kept[1].Appended}; !slices.Equal(middle.Launch.LeadMarks, want) {
		t.Errorf("the job whose brief could not read the leads marks %v; want %v, the first answer, which its "+
			"brief was to tell again, and its own", middle.Launch.LeadMarks, want)
	}

	// CUT OFF IN TURN: its copy is the first copy carried on, and may lack both
	// briefs.
	middle.Launch.HarnessEnd = sandbox.HarnessEndCutOff
	brief := relaunchHanded(t, again.launcher.company, again.handle, again.launcher.turn,
		runnerResume(resumeInput{Run: middle, State: state, Leads: read,
			Rotated: &runner.Rotated{SessionID: "sess-ask", BoxKept: true}}, nil, nil, testLeads())).Continue.Brief
	told := strings.Index(brief, leadsHeading)
	if told < 0 || !strings.Contains(brief[told:], "founder answered: use the release branch") ||
		!strings.Contains(brief[told:], "founder answered: the unit tests") {
		t.Errorf("the copy two cut-offs on, whose middle brief could not read the leads, is not told both "+
			"answers:\n%s", brief)
	}
}

// A BASE CONTINUATION REFUSED BEFORE ITS HARNESS WROTE IS TOLD ITS TURN AGAIN,
// end to end: a turn's first launch continues the session its conversation's
// last turn left, its brief where the turn begins and the turn's task, and the
// launch keeps that task beside the run's record as the lead the job's brief
// opened with. Its sign-in is
// refused at once, before its harness wrote the brief, so the copy in its box
// holds the turn before and none of this turn's task: the relaunch carrying the
// round on in that copy, told only to continue from where it is, would carry
// the turn before on. It is told the turn's task again, where the turn began,
// and nothing of the new box that first brief spoke of.
//
// Mutation: keep no lead for the base's brief, or hand the resume no base, and
// the copy is not told where its turn began.
func TestABaseContinuationRefusedBeforeItsHarnessWroteIsToldItsTurnAgain(t *testing.T) {
	t.Parallel()
	k := newConversingLaunch(t, 0, 0)
	k.log.endedOn(k.key, "t-1", "s-1", "T1's transcript", 42000)
	l := k.launcher("t2", k.read("t2"))
	first, err := k.c.RunnerFor(k.ctx, k.seat.Handle(), tools.NewRegistry(), RunnerInput{
		Task: "fix the failing test", Reply: turn.ToolReply(""), Launcher: l, Sessions: testSessions(t),
		Fence: k.e.seatFence(k.seat.Handle()),
		Turn:  runner.Turn{RunID: "t2", ConversationKey: k.key, Context: l.turn},
	})
	if err != nil {
		t.Fatalf("RunnerFor: %v", err)
	}
	if _, _, err := first.Execute(k.ctx, 1, "", nil); err != nil {
		t.Fatalf("the turn's first launch: %v", err)
	}
	pending := k.e.sandbox.Load().pending
	row, _, err := pending.Get(k.ctx, "t2")
	if err != nil {
		t.Fatal(err)
	}
	leads, err := pending.Leads(k.ctx, row)
	if err != nil {
		t.Fatal(err)
	}
	if row.Launch.Brief != sandbox.BriefContinuedConversation || len(leads.Kept) != 1 ||
		leads.Kept[0].Kind != sandbox.LeadKindTask || !slices.Equal(row.Launch.LeadMarks, []int{1}) {
		t.Fatalf("the first launch records brief %q and marks %v beside leads %+v; want the conversation's "+
			"session continued, its task the lead, marked", row.Launch.Brief, row.Launch.LeadMarks, leads.Kept)
	}

	// REFUSED AT ONCE, ITS HARNESS NEVER HAVING WRITTEN THE BRIEF: its harness
	// reached the job's tools, so its box said it continued the session, and
	// the model refused its first request.
	row.Launch.SessionStart, row.Launch.HarnessEnd = sandbox.SessionStartContinued, sandbox.HarnessEndCutOff
	session := row.SessionRan()
	if session == "" {
		t.Fatalf("the row names no session its job ran: %+v", row)
	}
	brief := relaunchHanded(t, k.c, k.seat.Handle(), l.turn, runnerResume(resumeInput{Run: row, Leads: leads,
		State:   execstate.State{Version: execstate.Version, Round: 1},
		Rotated: &runner.Rotated{SessionID: session, BoxKept: true}}, nil, nil, testLeads())).Continue.Brief
	told := strings.Index(brief, leadsHeading)
	if told < 0 {
		t.Fatalf("the copy is told nothing of the turn it may lack:\n%s", brief)
	}
	for _, want := range []string{"### A new turn of this conversation began here", "through turn `t-1`",
		"fix the failing test"} {
		if !strings.Contains(brief[told:], want) {
			t.Errorf("the turn told again does not say %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "new box") || strings.Contains(brief, "FRESH checkout") {
		t.Errorf("the turn told again speaks of a new box:\n%s", brief)
	}
}

// A RESUME'S LEADS REACH ITS RELAUNCH'S BRIEF, through the engine's resumer and
// the runner: the coordinator reads the turn's leads beside the run's record
// inside the resume's frame (sandbox.ResumeRequest.Leads, which
// TestAResumeIsHandedTheLeadsBesideItsRowAndEveryOneItCannotRead holds), the
// engine's resumer hands them to the resumed turn (resumeInput.Leads), and
// runnerResume opens them for the runner, which tells again the ones the job's
// brief told to a copy of its session that may lack that brief. So a job whose
// brief opened with an answer and whose harness was cut off, carried on in its
// own box by a person's answer to the question it then asked, is told the
// earlier answer again, and the relaunched job's record marks it told again
// in turn.
//
// THE CASE'S JOB stands for one whose brief continued the turn's session and
// opened with an answer, a relaunch carrying a round on: a turn's first launch,
// whose brief is fresh, opens with nothing a session lacks. So its attach is
// made again by the case, through the real take, as such a job's own launch
// makes it: the row's own box and command, the session its box ran, its brief
// one that continued, and the answer it opened with. And the resume is the one
// a person's answer drives, since the copy a job's box holds is continued in
// place by an answer's relaunch and never by a loss's (runner.Lost).
//
// Mutation: drop the leads in the engine's resumer, or hand runnerResume none,
// and the relaunch is told nothing of the answer.
func TestAResumesLeadsReachItsRelaunchsBrief(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	const turnID = "run-leads-to-brief"
	e, _ := spendingEngine(t)
	e.leads = testLeads()
	rig := rigRunsOn(t, e, sandbox.NewCoordStore(e.backends.Fleet), modelPlayer(t, billingRun))
	res, err := e.runTurn(ctx, Request{
		Handle: "swe", WorkKey: "wk-leads", RunID: turnID, WorkSince: time.Now().UTC(),
	})
	if err != nil || !res.Suspended {
		t.Fatalf("runTurn = %+v, %v; want the turn parked on its executor's run", res, err)
	}
	first := rig.row(t, turnID)
	pending := e.sandbox.Load().pending
	lead, err := testLeads().Seal(turnID, sandbox.Lead{Kind: sandbox.LeadKindAnswer, Job: first.LaunchID,
		Seq: first.LaunchSeq, Round: 1, Answer: &sandbox.LeadAnswer{Question: "which branch?",
			Words: "use the release branch", By: "founder"}})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	session := first.SessionID
	if session == "" {
		session = "sess-of-the-job"
	}
	if err := pending.AttachSandbox(ctx, turnID, sandbox.BoxRef{SandboxID: first.SandboxID,
		CommandID: first.CommandID, Placement: first.Placement, Node: first.LaunchNode, Harness: first.Harness,
		SessionID: session, PauseTTLSec: first.PauseTTLSeconds,
		FreshSessionID: first.Launch.FreshSessionID, Brief: sandbox.BriefContinued,
		BriefOmitted: first.Launch.BriefOmitted, FreshOmitted: first.Launch.FreshOmitted,
		Leads: sandbox.BriefLeads{Run: first.LeadsRun, Job: first.LaunchID, Counted: first.LeadsAppended,
			Own: []sandbox.KeptLead{lead}}},
		sandbox.Fence{Owner: first.Owner, Epoch: first.OwnerEpoch}); err != nil {
		t.Fatalf("the job's attach of the answer its brief opened with: %v", err)
	}
	// ITS HARNESS CUT OFF, which stands whatever the collection later reads:
	// the copy in its box may lack its brief.
	if marked, err := pending.MarkHarnessEnd(ctx, turnID, first.LaunchID,
		sandbox.EndRead{End: sandbox.HarnessEndCutOff}); err != nil || !marked {
		t.Fatalf("MarkHarnessEnd = %v, %v", marked, err)
	}
	// THE RESUME A PERSON'S ANSWER DRIVES, on the row its claim took, its leads
	// read beside the row as the coordinator reads them inside the resume's
	// frame.
	row, claimed, err := pending.ClaimForResume(ctx, turnID, sandbox.CompletionTail(first.LaunchID),
		sandbox.Fence{Owner: first.Owner, Epoch: first.OwnerEpoch})
	if err != nil || !claimed {
		t.Fatalf("ClaimForResume = %v, %v", claimed, err)
	}
	leads, err := pending.Leads(ctx, row)
	if err != nil || len(leads.Kept) != 1 {
		t.Fatalf("the run's leads read %+v, %v; want the job's answer", leads, err)
	}
	if err := (&resumer{engine: e}).Resume(ctx, sandbox.ResumeRequest{Run: row, Leads: leads,
		Asked: &sandbox.AskedRun{Question: "which test suite?", Answer: "the unit tests", By: "founder",
			SessionID: row.SessionRan(), BoxKept: true}}); err != nil {
		t.Fatalf("the answer's resume: %v", err)
	}
	started := rig.runners[first.Harness].Started()
	if len(started) != 2 || started[1].Resume != session {
		t.Fatalf("the harness was started %d times, the last continuing %q; want the relaunch continuing the "+
			"job's own session, %s, in its box", len(started), started[len(started)-1].Resume, session)
	}
	brief := started[1].Brief
	told := strings.Index(brief, leadsHeading)
	if told < 0 || !strings.Contains(brief[told:], "founder answered: use the release branch") {
		t.Errorf("the relaunch is not told the answer its copy may lack:\n%s", brief)
	}
	next := rig.row(t, turnID)
	if next.LaunchID == first.LaunchID || !slices.Contains(next.Launch.LeadMarks, 1) {
		t.Errorf("the relaunched job %s marks %v, want a new job marking the answer it told again, 1",
			next.LaunchID, next.Launch.LeadMarks)
	}
}
