package runner_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/toolname"
)

// WHAT A ROUND CARRIED ON IN ITS JOB'S OWN BOX TELLS THE SESSION THERE. A
// person's answer or a replaced sign-in that takes up no kept capture of the job
// continues the copy of the session the job's box holds, which holds every call
// the job made and every note it read only where its harness ended its own run
// ([runner.Resume.HarnessEnd]): a harness killed, crashed or told to stop can
// make a call and have it answered before it writes the call to its record, and
// no copy is known to hold the message its job was handed.

// cutOffHeader opens the calls a session cut off is told it may not show.
const cutOffHeader = "## Calls this run made that the session above may not show"

// notesAgainHeader opens the notes a session cut off is told again.
const notesAgainHeader = "## Notes this turn read that the session above may not show"

// heldSubmissionLine is what a continuation says first where the session it
// continues holds a successful `submit_work` of its round.
const heldSubmissionLine = "A `" + toolname.SubmitWork + "` the session above shows does NOT stand as this" +
	" work's submission: the work was carried on after it. When your work is done, call `" +
	toolname.SubmitWork + "` yourself."

// inBoxCarry is what carries a round on in its job's own box: name says which,
// set puts it on the resume, its job's box kept or gone, and lead is what the
// brief it is carried on with opens with.
type inBoxCarry struct {
	name string
	set  func(r *runner.Resume, boxKept bool)
	lead string
}

// inBoxCarryNamed is the one of [inBoxCarries] called name, failing t where
// none is: a case that picked none would read no brief, and pass.
func inBoxCarryNamed(t *testing.T, name string) inBoxCarry {
	t.Helper()
	for _, carry := range inBoxCarries {
		if carry.name == name {
			return carry
		}
	}
	t.Fatalf("no carry named %q", name)
	return inBoxCarry{}
}

// inBoxCarries are both: a replaced sign-in, and a person's answer.
var inBoxCarries = []inBoxCarry{
	{name: "a replaced sign-in", lead: runner.RotatedContinuation,
		set: func(r *runner.Resume, boxKept bool) {
			r.Rotated = &runner.Rotated{SessionID: "sess-1", BoxKept: boxKept}
		}},
	{name: "a person's answer", lead: "## Your question was answered",
		set: func(r *runner.Resume, boxKept bool) {
			r.Asked = &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder",
				SessionID: "sess-1", BoxKept: boxKept}
		}},
}

// inPlaceBrief is the brief carry carries a round on with in its job's own box —
// the launcher continuing no kept session — where the job's session opened once
// the round held from calls, boxKept says its box is still there, and with tells
// the resume what else the job's record says: the round's calls, the notes the
// turn read, how its harness ended. box is where the turn's notes wait, nil for
// none.
func inPlaceBrief(t *testing.T, carry inBoxCarry, boxKept bool, from int, box runner.SteerSource,
	with func(*runner.Resume)) string {

	t.Helper()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools, parked.State.BridgeFrom = req.Surface.Active(), from
	resume := &runner.Resume{State: parked.State}
	carry.set(resume, boxKept)
	with(resume)
	relauncher := &playLauncher{}
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
		steer: box, resume: resume})
	if _, _, err := collector.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again := relauncher.last(t)
	if again.Session != nil || again.Continue == nil || again.Continue.SessionID != "sess-1" {
		t.Fatalf("the relaunch continues kept %+v and in place %+v, want the job's own session in its box",
			again.Session, again.Continue)
	}
	if !strings.HasPrefix(again.Continue.Brief, carry.lead) {
		t.Fatalf("the session is not led by what carries it on, %q:\n%s", carry.lead, again.Continue.Brief)
	}
	return again.Continue.Brief
}

// A ROUND CARRIED ON IN ITS JOB'S BOX IS TOLD IT WAS CUT OFF AND EVERY CALL OF THE
// ROUND. Where the job's record does not say its harness ended its own run, the
// session in its box is told, after what carries it on — a replaced sign-in or a
// person's answer alike — that it was cut off where the record says so, and that
// it may have been where the record says nothing (an older build collected the
// job, or the record did not land) or says it in a word this build does not
// know, in words that say nothing of a save, whether or not any call is listed;
// and then, under the header of a session cut off, EVERY CALL OF THE ROUND, with
// every call the log dropped from its middle counted: never only the job's own,
// since the copy is not known to hold the job's brief, which told the round's
// calls before the job's own. A job whose harness ended its own run is told none
// of it, its copy holding them all; nor is one whose box is gone, which no launch
// continues in.
//
// Mutation: tell nothing in place, tell only the job's own calls where it made
// one, tell the calls where the harness ended its own run or its box is gone,
// tell them under the header of a session saved where it ends, leave the cut-off
// section off, say it only where a call is listed, or say the copy was cut off
// where its record says nothing, and a case goes red.
func TestARunCarriedOnInItsBoxIsToldTheCallsItsCopyMayLack(t *testing.T) {
	t.Parallel()
	earlier := numbered("jira_create", map[string]any{"summary": "an earlier job's issue"}, 1, 1)
	another := numbered("jira_create", map[string]any{"summary": "another earlier job's issue"}, 2, 2)
	own := numbered("slack_post", map[string]any{"text": "the job's own post"}, 2, 2)
	asked := numbered(toolname.AskAndWait, map[string]any{"question": "the job's own question"}, 2, 2)
	dropped := []ledger.Elided{{Name: "slack_post", Calls: 5}}
	for name, tc := range map[string]struct {
		from      int
		bridged   []runner.BridgedCall
		elided    []ledger.Elided
		end       sandbox.HarnessEnd
		boxGone   bool
		told, not []string
		dropped   string
		// section is the cut-off section the copy is told, "" for none.
		section string
	}{
		"a job recorded cut off, after a call of its own": {from: 1, end: sandbox.HarnessEndCutOff,
			bridged: []runner.BridgedCall{earlier, own}, section: prompts.InterruptedCopy,
			told: []string{"an earlier job's issue", "the job's own post"}},
		"a job whose record says nothing, after a call of its own": {from: 1,
			bridged: []runner.BridgedCall{earlier, own}, section: prompts.MayBeInterruptedCopy,
			told: []string{"an earlier job's issue", "the job's own post"}},
		"a job whose record holds a word this build does not know": {from: 1, end: "a-later-builds-end",
			bridged: []runner.BridgedCall{earlier, own}, section: prompts.MayBeInterruptedCopy,
			told: []string{"an earlier job's issue", "the job's own post"}},
		"a job whose one call of its own was its question": {from: 1, end: sandbox.HarnessEndCutOff,
			bridged: []runner.BridgedCall{earlier, asked}, section: prompts.InterruptedCopy,
			told: []string{"an earlier job's issue", "the job's own question"}},
		"a job cut off before it made a call of its own": {from: 2, end: sandbox.HarnessEndCutOff,
			bridged: []runner.BridgedCall{earlier, another}, section: prompts.InterruptedCopy,
			told: []string{"an earlier job's issue", "another earlier job's issue"}},
		"a job cut off in a round whose log dropped calls": {from: 1, end: sandbox.HarnessEndCutOff,
			bridged: []runner.BridgedCall{earlier, numbered("slack_post",
				map[string]any{"text": "the job's own post"}, 8, 7)}, elided: dropped,
			section: prompts.InterruptedCopy,
			told:    []string{"an earlier job's issue", "the job's own post"},
			dropped: "(+5 further calls this run made since that point"},
		"a job cut off in a round that made no call": {from: 0, end: sandbox.HarnessEndCutOff,
			section: prompts.InterruptedCopy, not: []string{cutOffHeader}},
		"a job whose harness ended its own run": {from: 1, end: sandbox.HarnessEndOwnRun,
			bridged: []runner.BridgedCall{earlier, own}},
		"a job cut off whose box is gone": {from: 1, end: sandbox.HarnessEndCutOff, boxGone: true,
			bridged: []runner.BridgedCall{earlier, own}},
	} {
		for _, carry := range inBoxCarries {
			t.Run(carry.name+", "+name, func(t *testing.T) {
				t.Parallel()
				brief := inPlaceBrief(t, carry, !tc.boxGone, tc.from, nil, func(r *runner.Resume) {
					r.Bridged, r.Elided, r.HarnessEnd = tc.bridged, tc.elided, tc.end
				})
				for _, section := range []string{prompts.InterruptedCopy, prompts.MayBeInterruptedCopy} {
					if section != tc.section && strings.Contains(brief, section) {
						t.Errorf("the copy is told %q, want %q:\n%s", section, tc.section, brief)
					}
				}
				if tc.section == "" {
					for _, not := range []string{"an earlier job's issue", "the job's own post", cutOffHeader} {
						if strings.Contains(brief, not) {
							t.Errorf("a copy whose harness ended its own run, or whose box is gone, is told "+
								"%q:\n%s", not, brief)
						}
					}
					if carry.lead == runner.RotatedContinuation && brief != carry.lead {
						t.Errorf("the copy is told more than what carries it on:\n%s", brief)
					}
					return
				}
				cut := strings.Index(brief, tc.section)
				if cut < 0 {
					t.Errorf("a copy not known to have ended its own run is not told %q:\n%s", tc.section, brief)
				}
				if len(tc.told) > 0 && strings.Index(brief, cutOffHeader) < cut {
					t.Errorf("the calls are not told after the cut-off section, as ones the session may not "+
						"show:\n%s", brief)
				}
				for _, want := range tc.told {
					if !strings.Contains(brief, want) {
						t.Errorf("the session is not told %q:\n%s", want, brief)
					}
				}
				for _, not := range tc.not {
					if strings.Contains(brief, not) {
						t.Errorf("the session is told %q:\n%s", not, brief)
					}
				}
				tally := ledger.FormatElided(dropped, nil)
				switch at := strings.Index(brief, tc.dropped); {
				case tc.dropped == "" && strings.Contains(brief, tally):
					t.Errorf("the session is told of dropped calls where none were:\n%s", brief)
				case tc.dropped != "" && (at < 0 || strings.Index(brief, tally) < at):
					t.Errorf("the session is not told %q followed by the round's tally:\n%s", tc.dropped, brief)
				}
			})
		}
	}
}

// AN APPROVED CALL CARRIED ON IN ITS BOX IS TOLD FIRST, AND THEN WHAT ITS COPY MAY
// LACK. A person's answer that approves a held call makes the call and carries
// the round on in the asking job's own box, its brief led by what the call
// returned; where the job's record does not say its harness ended its own run,
// what the copy may lack follows that lead — the cut-off section, then every
// call of the round — never ahead of it.
//
// Mutation: tell an answer's copy nothing in place, or tell what it may lack
// ahead of what carries it on, and the case goes red.
func TestAnApprovedCallCarriedOnInItsBoxIsToldFirstThenWhatItsCopyMayLack(t *testing.T) {
	t.Parallel()
	r, tool, _, launcher, _ := heldCarry(t, heldState(), []runner.BridgedCall{skillLoad()})
	if _, _, err := r.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if made := tool.made(); made != 1 {
		t.Fatalf("the approved call ran %d time(s), want once", made)
	}
	again := launcher.last(t)
	if again.Continue == nil {
		t.Fatal("the answer carries nothing on in the asking job's box")
	}
	brief := again.Continue.Brief
	lead := strings.Index(brief, "The call was made, and returned:")
	cut := strings.Index(brief, prompts.MayBeInterruptedCopy)
	calls := strings.Index(brief, cutOffHeader)
	if lead < 0 || cut < lead || calls < cut {
		t.Fatalf("the brief tells what the call returned at %d, the cut-off section at %d and the round's "+
			"calls at %d, want them in that order:\n%s", lead, cut, calls, brief)
	}
	if !strings.Contains(brief[calls:], "tracker-rules") {
		t.Errorf("the round's calls told do not list the asking job's load:\n%s", brief[calls:])
	}
}

// A ROUND CARRIED ON IN ITS JOB'S BOX IS TOLD AGAIN EVERY NOTE THE TURN HAS READ.
// A copy whose harness was cut off may lack its job's last take of the notes,
// recorded read before its answer reached the harness, and is not known to hold
// its job's brief, which told the notes the session it continued lacked — an
// earlier job's among them — beside the ones it was the first to read. No record
// says from where that brief told them, so where the job's record does not say
// its harness ended its own run the session in its box is told again, after the
// calls of the round and before the notes waiting for it, every note the turn
// has read, once each, in the order read, under a header that counts them and
// says a note shown both above and there is one note — a second copy of one,
// unheaded, would read as a note of its own; the notes waiting follow, once. A
// copy whose harness ended its own run holds them all, and is told only the
// notes waiting — but for a read the record marks unanswered, a take whose
// answer never reached its harness: the copy lacks that note and holds the
// answered ones after it, so it is told every note from that read on, under the
// same header, and none before.
//
// Mutation: tell the notes from the job's first read, tell none, tell them where
// the harness ended its own run with every take answered, leave a read marked
// unanswered untold where it did, tell them from the last read marked rather
// than the first, tell the notes before it, tell them without their header,
// after the waiting notes, or a second time with no header, and a case goes
// red.
func TestARunCarriedOnInItsBoxIsToldAgainEveryNoteTheTurnRead(t *testing.T) {
	t.Parallel()
	read := func(id, text string, order int, boundary types.SteerBoundary, launch string) steer.Read {
		return steer.Read{Note: steer.Note{ID: id, Text: text, By: "founder"},
			Reading: steer.Reading{Phase: types.PhaseExecute, Iteration: 1, Round: 1, Boundary: boundary,
				Launch: launch, Order: order}}
	}
	steered := []steer.Read{
		read("earlier", "an earlier job's note", 1, types.SteerBoundaryToolBatch, "job-0"),
		read("brief", "the note the job's brief carried", 2, types.SteerBoundaryBrief, "job-1"),
		read("take", "the note the job's last take read", 3, types.SteerBoundaryToolBatch, "job-1"),
	}
	msg := func(r steer.Read) string { return prompts.SteerMessage(r.By, r.Text) }
	waiting := steer.Note{ID: "waiting", Text: "the note still waiting", By: "founder"}
	call := numbered("jira_create", map[string]any{"summary": "an earlier job's issue"}, 1, 1)
	// A TAKE OF THE JOB'S WHOSE ANSWER WAS LOST, and an answered one after it.
	lost := read("lost", "the note whose take's answer was lost", 3, types.SteerBoundaryToolBatch, "job-1")
	lost.Unanswered = true
	unanswered := []steer.Read{steered[0], steered[1], lost,
		read("after", "the note an answered take read after it", 4, types.SteerBoundaryToolBatch, "job-1")}
	// AND A LATER TAKE OF IT LOST TOO, the answered one between them.
	lostAgain := read("lost-again", "the note a later take lost", 5, types.SteerBoundaryToolBatch, "job-1")
	lostAgain.Unanswered = true
	twice := append(append([]steer.Read{}, unanswered...), lostAgain)
	for name, tc := range map[string]struct {
		job       string
		steered   []steer.Read
		end       sandbox.HarnessEnd
		told, not []string
		count     string
	}{
		"a job cut off": {job: "job-1", steered: steered, end: sandbox.HarnessEndCutOff,
			told: []string{msg(steered[0]), msg(steered[1]), msg(steered[2])}, count: "the 3 notes below"},
		// THE NOTE AN EARLIER JOB TOOK LAST, which the job's own brief told its
		// copy again, while the job itself read none.
		"a job that read no note, after an earlier job's take": {job: "job-2", steered: steered[:1],
			told: []string{msg(steered[0])}, count: "the note below"},
		"a job whose harness ended its own run": {job: "job-1", steered: steered, end: sandbox.HarnessEndOwnRun,
			not: []string{msg(steered[0]), msg(steered[1]), msg(steered[2]), notesAgainHeader}},
		"a job whose harness ended its own run, a take of it unanswered": {job: "job-1", steered: unanswered,
			end: sandbox.HarnessEndOwnRun, told: []string{msg(unanswered[2]), msg(unanswered[3])},
			not: []string{msg(unanswered[0]), msg(unanswered[1])}, count: "the 2 notes below"},
		"a job whose harness ended its own run, two takes of it unanswered around an answered one": {
			job: "job-1", steered: twice, end: sandbox.HarnessEndOwnRun,
			told: []string{msg(twice[2]), msg(twice[3]), msg(twice[4])},
			not:  []string{msg(twice[0]), msg(twice[1])}, count: "the 3 notes below"},
	} {
		for _, carry := range inBoxCarries {
			t.Run(carry.name+", "+name, func(t *testing.T) {
				t.Parallel()
				box := steer.New()
				box.Offer(waiting)
				brief := inPlaceBrief(t, carry, true, 1, box, func(r *runner.Resume) {
					r.Bridged, r.Steered, r.HarnessEnd = []runner.BridgedCall{call}, tc.steered, tc.end
					r.Run = runner.RunRecord{LaunchID: tc.job}
				})
				waitingMsg := prompts.SteerMessage(waiting.By, waiting.Text)
				last := strings.Index(brief, waitingMsg)
				if last < 0 || strings.Count(brief, waitingMsg) != 1 {
					t.Fatalf("the session is not told the note waiting for it once:\n%s", brief)
				}
				for _, not := range tc.not {
					if strings.Contains(brief, not) {
						t.Errorf("the session is told %q again:\n%s", not, brief)
					}
				}
				if len(tc.told) == 0 {
					return
				}
				header := strings.Index(brief, notesAgainHeader)
				if header < strings.Index(brief, cutOffHeader) || !strings.Contains(brief[max(header, 0):], tc.count) {
					t.Errorf("the notes told again are not headed, after the round's calls, as %s the "+
						"session may not show:\n%s", tc.count, brief)
				}
				at := header
				for _, want := range tc.told {
					i := strings.Index(brief, want)
					if i < 0 || i < at || i > last || strings.Count(brief, want) != 1 {
						t.Errorf("the session is not told %q again once, under the header, in the order read "+
							"and before the note waiting:\n%s", want, brief)
					}
					at = i
				}
			})
		}
	}
}

// A ROUND CARRIED ON IN ITS JOB'S BOX ARMS THE JOB'S OWN LOADS ONLY WHERE ITS
// HARNESS ENDED ITS OWN RUN. The copy in the job's box holds the skills the job's
// launch armed, the session it continued having held their bodies before the
// job began, and the job's own loads only where its harness wrote every call it
// made: one cut off may never have written the load, which it is told as a call
// it may lack, so the skill is loaded again before a call it covers — the safe
// direction, never a call admitted on a body its session may not hold.
//
// Mutation: arm the job's own loads whatever its harness's end, and the cut-off
// row is admitted; arm a cut-off copy with nothing, and the launch's own skill
// is refused.
func TestARunCarriedOnInItsBoxArmsItsJobsLoadsOnlyWhereItsHarnessEndedItsOwnRun(t *testing.T) {
	t.Parallel()
	inBox := runner.SessionBasis{Why: "the newest capture of the job was not kept: refused"}
	active := guarded{basis: inBox}.relaunch(t).Surface.Active()
	carried := func(end sandbox.HarnessEnd) func(*runner.Resume) {
		return func(r *runner.Resume) {
			r.Rotated = &runner.Rotated{SessionID: "sess-1", BoxKept: true}
			r.HarnessEnd = end
		}
	}
	for name, tc := range map[string]struct {
		g       guarded
		refused bool
	}{
		"its job loaded the skill, and its harness ended its own run": {g: guarded{basis: inBox,
			state: execstate.State{ActiveTools: active}, bridged: []runner.BridgedCall{loadSkill(1)},
			carry: carried(sandbox.HarnessEndOwnRun)}},
		"its job loaded the skill, and its harness may have been cut off": {g: guarded{basis: inBox,
			state: execstate.State{ActiveTools: active}, bridged: []runner.BridgedCall{loadSkill(1)},
			carry: carried(sandbox.HarnessEndCutOff)}, refused: true},
		"its launch armed the skill, and its harness may have been cut off": {g: guarded{basis: inBox,
			continued: true, state: execstate.State{ActiveTools: active, LoadedSkills: []string{"chat-conventions"}},
			carry: carried("")}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := tc.g.relaunch(t)
			if req.Session != nil || req.Continue == nil || req.Continue.SessionID != "sess-1" {
				t.Fatalf("the relaunch continues kept %+v and in place %+v, want the job's own session in its box",
					req.Session, req.Continue)
			}
			bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})
			if bridged[0].Failed != tc.refused {
				t.Fatalf("slack_post failed %v (%q), want refused %v", bridged[0].Failed, bridged[0].Result,
					tc.refused)
			}
		})
	}
}

// A ROUND CARRIED ON IN ITS JOB'S BOX LAUNCHES NOTHING WHEN THE CALLS ITS COPY MAY
// LACK FILL THE WINDOW, AND CHARGES NOTHING FOR THEM WHERE THE BOX IS GONE.
// Telling the copy the round's calls rewrites a payload past its budget with the
// seat's auxiliary model, charged to the turn's counters, and that rewrite can
// take the window past its ceiling: the relaunch asks the meter once every
// rewrite its briefs make is spent, before anything is launched, so a run the
// rewrite left no room for is never launched and paid for whole past the
// ceiling. Where the job's box is gone no launch continues in it, so its calls
// are neither rewritten nor charged, and the relaunch is not refused for a brief
// nothing would send.
//
// Mutation: build the calls the copy may lack after the relaunch asks the meter,
// and the run whose box is kept is launched; build them whether or not the box
// is kept, and the run whose box is gone is charged and refused.
func TestARunCarriedOnInItsBoxLaunchesNothingWhenTheCallsItsCopyMayLackFillTheWindow(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		boxKept bool
		charges []int
		refused bool
	}{
		"its box kept": {boxKept: true, charges: []int{150}, refused: true},
		"its box gone": {boxKept: false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
			launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
			parked, _ := launched.Suspended()
			parked.State.ActiveTools = req.Surface.Active()
			// ONE CALL OF THE ROUND, its body past the ledger's value budget.
			bridged := []runner.BridgedCall{numbered("jira_create",
				map[string]any{"summary": "before the refusal", "body": strings.Repeat("b", 2000)}, 1, 1)}
			meter := &ceilingMeter{ceiling: 100}
			relauncher := &playLauncher{}
			collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
				budget: meter, compact: compact.New(chargingModels{meter: meter, tokens: 150}, nil).
					For(nil, auxspend.Use{Stage: types.AuxStageTurn}),
				resume: &runner.Resume{State: parked.State, Bridged: bridged,
					Rotated: &runner.Rotated{SessionID: "sess-1", BoxKept: tc.boxKept}}})

			_, _, err := collector.Resume(t.Context(), nil)
			if refused := errors.Is(err, toolloop.ErrBudgetExhausted); refused != tc.refused || (!refused && err != nil) {
				t.Fatalf("Resume = %v, want refused for the window %v", err, tc.refused)
			}
			if got := meter.charges(); len(got) != len(tc.charges) || (len(got) > 0 && got[0] != tc.charges[0]) {
				t.Fatalf("charges = %v, want %v", got, tc.charges)
			}
			if n := relauncher.launched(); (n == 0) != tc.refused {
				t.Errorf("relaunched %d runs, want a launch %v", n, !tc.refused)
			}
		})
	}
}

// A ROUND CARRIED ON IN ITS JOB'S BOX IS TOLD A NOTE ITS HARNESS WAS SERVED AS
// ONE IT MAY ALREADY SHOW. A note the run's bridge served the job's OpenCode
// harness is recorded read only once the harness confirms delivering it, so the
// copy in the job's box may hold one the record says still waits: it is told
// it under the header of notes it may already show, with every note waiting
// before it — the order read kept — and the notes after it plainly; where its
// harness was cut off, after every note the turn has read, under the same
// header.
//
// Mutation: tell a served note plainly, take the notes after it under the
// header, or tell it apart from the notes the turn has read where the copy was
// cut off, and a case goes red.
func TestARunCarriedOnInItsBoxIsToldANoteItsHarnessWasServedAsOneItMayAlreadyShow(t *testing.T) {
	t.Parallel()
	read := steer.Read{Note: steer.Note{ID: "read", Text: "the note the job's take read", By: "founder"},
		Reading: steer.Reading{Phase: types.PhaseExecute, Iteration: 1, Round: 1,
			Boundary: types.SteerBoundaryStep, Launch: "job-1", Order: 1}}
	note := func(id, text string) steer.Note { return steer.Note{ID: id, Text: text, By: "founder"} }
	early := note("early", "the note waiting, never served")
	served := note("served", "the note served to the job's harness, its confirmation lost")
	late := note("late", "the note filed after it")
	msg := func(n steer.Note) string { return prompts.SteerMessage(n.By, n.Text) }
	call := numbered("jira_create", map[string]any{"summary": "an earlier job's issue"}, 1, 1)
	for name, tc := range map[string]struct {
		end          sandbox.HarnessEnd
		again, plain []string
		count        string
	}{
		"a job whose harness ended its own run": {end: sandbox.HarnessEndOwnRun,
			again: []string{msg(early), msg(served)}, plain: []string{msg(late)}, count: "the 2 notes below"},
		"a job cut off": {end: sandbox.HarnessEndCutOff,
			again: []string{msg(read.Note), msg(early), msg(served)}, plain: []string{msg(late)},
			count: "the 3 notes below"},
	} {
		for _, carry := range inBoxCarries {
			t.Run(carry.name+", "+name, func(t *testing.T) {
				t.Parallel()
				box := steer.New()
				for _, n := range []steer.Note{early, served, late} {
					box.Offer(n)
				}
				brief := inPlaceBrief(t, carry, true, 1, box, func(r *runner.Resume) {
					r.Bridged, r.Steered, r.HarnessEnd = []runner.BridgedCall{call}, []steer.Read{read}, tc.end
					r.Served, r.Run = []string{"served"}, runner.RunRecord{LaunchID: "job-1"}
				})
				header := strings.Index(brief, notesAgainHeader)
				if header < 0 || !strings.Contains(brief[header:], tc.count) {
					t.Fatalf("the notes it may already show are not headed as %s:\n%s", tc.count, brief)
				}
				if tc.end.EndedOwnRun() && strings.Contains(brief, msg(read.Note)) {
					t.Errorf("a copy whose harness ended its own run is told the note it read:\n%s", brief)
				}
				at := header
				for _, want := range append(append([]string{}, tc.again...), tc.plain...) {
					i := strings.Index(brief, want)
					if i < 0 || i < at || strings.Count(brief, want) != 1 {
						t.Errorf("the session is not told %q once, in the order read:\n%s", want, brief)
					}
					at = i
				}
			})
		}
	}
}

// A CUT-OFF COPY IS TOLD ITS EARLIER SUBMIT_WORK DOES NOT STAND. A run may
// submit and then ask, since its gate refuses a submission after its question
// and never a question after a submission, and every call a brief tells was
// made before the relaunch it opens: the asking job's own calls fall below the
// cut its answer's opening takes, and a round refused on a replaced sign-in
// whose calls since its last answer hold a submission is collected on it
// rather than carried on, so the one such a carry tells was made before an
// earlier answer. Neither is the submission of the work the relaunch carries
// on. The copy a job's own box holds, its harness cut off, is told every call
// of the round, the submission among them as `→ success`; so right after those
// calls it is told, once, that the submission does not stand and that it makes
// its own, under a person's answer and a replaced sign-in alike. A submission
// the bridge refused is told as any failed call, without the line.
//
// Mutation: never say a successful submission is among the calls, or say it
// of a refused one too, and a case goes red.
func TestACutOffCopyIsToldItsEarlierSubmitWorkDoesNotStand(t *testing.T) {
	t.Parallel()
	const notStanding = "does NOT stand as this work's submission: the work was carried on after it."
	submission := func(failed bool) runner.BridgedCall {
		return runner.BridgedCall{Call: ledger.Call{Name: runner.SubmitWorkTool,
			Args:   map[string]any{"outcome": "delivered", "summary": "the asking job's submission"},
			Result: "submitted", Failed: failed}, Seq: 1, Appended: 1}
	}
	asked := numbered(toolname.AskAndWait, map[string]any{"question": "the job's own question"}, 2, 2)
	posted := numbered("slack_post", map[string]any{"text": "a post after the answer"}, 3, 3)
	// EACH CARRY'S OWN ROUND: a person's answer to the job that submitted and
	// then asked; and a replaced sign-in refusing the job that answer opened,
	// whose own call is a post, the submission before the answer's cut.
	rounds := map[string]struct {
		from, answered int
		after          []runner.BridgedCall
		last           string
	}{
		"a person's answer": {from: 0, after: []runner.BridgedCall{asked}, last: "the job's own question"},
		"a replaced sign-in": {from: 2, answered: 2, after: []runner.BridgedCall{asked, posted},
			last: "a post after the answer"},
	}
	for _, carry := range inBoxCarries {
		round, ok := rounds[carry.name]
		if !ok {
			t.Fatalf("no round for %s", carry.name)
		}
		for _, failed := range []bool{false, true} {
			name := carry.name + ", its submission made"
			if failed {
				name = carry.name + ", its submission refused"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				brief := inPlaceBrief(t, carry, true, round.from, nil, func(r *runner.Resume) {
					r.Bridged = append([]runner.BridgedCall{submission(failed)}, round.after...)
					r.Answered, r.HarnessEnd = round.answered, sandbox.HarnessEndCutOff
				})
				calls, listed := strings.Index(brief, cutOffHeader), strings.Index(brief, "the asking job's submission")
				if calls < 0 || listed < calls {
					t.Fatalf("the copy is not told the submission among the calls it may not show:\n%s", brief)
				}
				said := strings.Index(brief, notStanding)
				if failed {
					if said >= 0 {
						t.Errorf("a refused submission is told %q:\n%s", notStanding, brief)
					}
					return
				}
				if said < strings.Index(brief, round.last) || strings.Count(brief, notStanding) != 1 {
					t.Errorf("the copy is not told once, after the calls it may not show, that the submission among "+
						"them does not stand:\n%s", brief)
				}
				if !strings.Contains(brief[said:], "call `"+toolname.SubmitWork+"` yourself") {
					t.Errorf("the copy is not told to make its own submission:\n%s", brief)
				}
			})
		}
	}
}

// A COPY HOLDING ITS EARLIER SUBMIT_WORK IS TOLD IT DOES NOT STAND. A run may
// submit and then ask, or call a tool that waits for a person's approval, its
// harness then ending its own run as the result parking it tells it to; and a
// job its answer opened in that session may have its sign-in replaced. The
// copy in the box then shows the submission answered "submitted", while the
// cut the answer's opening takes falls past it, so it is no submission of the
// work the relaunch carries on. That copy holds every call of the round and is
// told none of them, so its brief was what carries it on alone, which says to
// finish the task as it asks. It is told FIRST, right after that lead and
// before any late call, once, that the submission the session shows does not
// stand and that it makes its own: under a person's answer, a held call's
// answer and a replaced sign-in alike. A submission the bridge refused is no
// submission, and nothing is said of it. A late one, or one nobody waited for,
// is held at no point: it is told in the late calls' section under the line on
// a late submission, and never under this one.
//
// Mutation: never say a held submission does not stand, say it of a refused,
// a late or an abandoned one, or say it after the late calls, and a case goes
// red.
func TestACopyHoldingItsEarlierSubmitWorkIsToldItDoesNotStand(t *testing.T) {
	t.Parallel()
	const lateHeading = "## Calls that finished after the run that made them stopped waiting for them"
	submission := func(failed bool) runner.BridgedCall {
		return runner.BridgedCall{Call: ledger.Call{Name: runner.SubmitWorkTool,
			Args:   map[string]any{"outcome": "delivered", "summary": "the asking job's submission"},
			Result: "submitted", Failed: failed}, Seq: 1, Appended: 1}
	}
	asked := numbered(toolname.AskAndWait, map[string]any{"question": "the job's own question"}, 2, 2)
	posted := numbered("slack_post", map[string]any{"text": "a post after the answer"}, 3, 3)
	answer := inBoxCarryNamed(t, "a person's answer")
	// saysHeld fails t unless brief, the copy's whole brief, lists no call of
	// the round and, where said, ends with the line that the submission the
	// session shows does not stand, said once after what carries it on; where
	// not said, nothing of a submission.
	saysHeld := func(t *testing.T, brief string, said bool) {
		t.Helper()
		if strings.Contains(brief, "the asking job's submission") || strings.Contains(brief, cutOffHeader) {
			t.Fatalf("a copy holding every call of the round is told one of them:\n%s", brief)
		}
		if !said {
			if strings.Contains(brief, "does NOT stand") {
				t.Errorf("a copy whose submission the bridge refused is told one does not stand:\n%s", brief)
			}
			return
		}
		if !strings.HasSuffix(brief, "\n\n"+heldSubmissionLine) || strings.Count(brief, heldSubmissionLine) != 1 {
			t.Errorf("the copy is not told once, after what carries it on, that the submission it shows does not "+
				"stand:\n%s", brief)
		}
	}
	// EACH CARRY'S OWN ROUND: a person's answer to the job that submitted and
	// then asked; and a replaced sign-in refusing the job that answer opened in
	// the same session, whose own call is a post.
	rounds := map[string]struct {
		from, answered int
		after          []runner.BridgedCall
	}{
		"a person's answer":  {after: []runner.BridgedCall{asked}},
		"a replaced sign-in": {from: 2, answered: 2, after: []runner.BridgedCall{asked, posted}},
	}
	for _, failed := range []bool{false, true} {
		made := ", its submission made"
		if failed {
			made = ", its submission refused"
		}
		for _, carry := range inBoxCarries {
			round, ok := rounds[carry.name]
			if !ok {
				t.Fatalf("no round for %s", carry.name)
			}
			t.Run(carry.name+made, func(t *testing.T) {
				t.Parallel()
				brief := inPlaceBrief(t, carry, true, round.from, nil, func(r *runner.Resume) {
					r.Bridged = append([]runner.BridgedCall{submission(failed)}, round.after...)
					r.Answered, r.HarnessEnd = round.answered, sandbox.HarnessEndOwnRun
				})
				saysHeld(t, brief, !failed)
			})
		}
		t.Run("a held call's answer"+made, func(t *testing.T) {
			t.Parallel()
			// THE JOB SUBMITTED, THEN CALLED A TOOL THAT WAITS FOR A PERSON'S
			// APPROVAL, which an attempt at the answer made.
			brief := laterAttemptBrief(t, runner.SessionBasis{}, sandbox.HarnessEndOwnRun, []runner.BridgedCall{
				submission(failed), heldFor("tr_close_issue", 2, 2), approvedFor("tr_close_issue", "held-1", 3, 3),
			}, nil)
			saysHeld(t, brief, !failed)
		})
	}
	// A LATE SUBMISSION, AND ONE NOBODY WAITED FOR, ARE HELD AT NO POINT.
	for name, mark := range map[string]func(*runner.BridgedCall){
		"a late submission":              func(c *runner.BridgedCall) { c.Late = true },
		"a submission nobody waited for": func(c *runner.BridgedCall) { c.Abandoned = true },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			late := submission(false)
			mark(&late)
			brief := inPlaceBrief(t, answer, true, 0, nil, func(r *runner.Resume) {
				r.Bridged, r.HarnessEnd = []runner.BridgedCall{late, asked}, sandbox.HarnessEndOwnRun
			})
			at := strings.Index(brief, lateHeading)
			if at < 0 || !strings.Contains(brief[at:], "the asking job's submission") ||
				!strings.Contains(brief[at:], "the run that made it had stopped waiting for it") {
				t.Fatalf("the submission is not told in the late calls' section, under its own line:\n%s", brief)
			}
			if strings.Contains(brief, heldSubmissionLine) {
				t.Errorf("a submission the copy is not known to hold is told as one it shows:\n%s", brief)
			}
		})
	}
	t.Run("a held submission beside a late call", func(t *testing.T) {
		t.Parallel()
		landed := numbered("slack_post", map[string]any{"text": "a post that landed late"}, 3, 3)
		landed.Late = true
		brief := inPlaceBrief(t, answer, true, 0, nil, func(r *runner.Resume) {
			r.Bridged, r.HarnessEnd = []runner.BridgedCall{submission(false), asked, landed}, sandbox.HarnessEndOwnRun
		})
		said, at := strings.Index(brief, heldSubmissionLine), strings.Index(brief, lateHeading)
		if said < 0 || at < said || strings.Count(brief, heldSubmissionLine) != 1 ||
			!strings.Contains(brief[at:], "a post that landed late") {
			t.Errorf("the copy is not told once, before the late calls, that the submission it shows does not "+
				"stand:\n%s", brief)
		}
	})
}
