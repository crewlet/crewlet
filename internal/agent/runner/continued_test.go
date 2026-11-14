package runner_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

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
	"github.com/crewlet/crewlet/internal/notify"
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

// markingBox is a box whose briefs say the run's record marks unanswered the
// reads it names: a take's settle that landed after the segment resumed from
// the record ([steer.Briefing.Unanswered]).
type markingBox struct {
	*steer.Box
	unanswered []string
}

func (m markingBox) Brief(ctx context.Context) steer.Briefing {
	b := m.Box.Brief(ctx)
	b.Unanswered = m.unanswered
	return b
}

// placingBox is a box whose briefs say where the run's record numbered the
// reads of the notes it names ([steer.Briefing.Orders]): the segment's own
// reads, and a note of the brief's a lost take recorded.
type placingBox struct {
	*steer.Box
	orders map[string]int
}

func (p placingBox) Brief(ctx context.Context) steer.Briefing {
	b := p.Box.Brief(ctx)
	b.Orders = p.orders
	return b
}

// keptBasis is a turn's kept session as a launcher answers it: captured once
// round 1's run had made its one call and read its first note.
var keptBasis = runner.SessionBasis{LaunchSeq: 1, CaptureSeq: 1, SessionID: "s-1",
	Through: 1, NoteCursor: 1, Home: "/boxes/4f2a/home", ContextTokens: 40000}

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
// run made through the seat's tools that the session may not have been
// answered — every call the round's log took after the read the session's save
// point is, so after the point it ends and one still in flight when it was
// saved, which the log took once its tool answered — with the call's outcome,
// never one the log took before that read. A session cut off is saved at the
// newest point its launch was known to hold, which may be short of where it
// ends — the round's start, where its launch landed no capture of its own — so
// those calls are told as ones it may already show, each shown both there and
// in the session one call, and never as calls it lacks. A round lost a second
// time starts a session of its own, rather than replay the state it was lost
// in toward the round's limit.
//
// Mutation: list every call of the round, read the save point against the
// order the calls were made in rather than the order the log took them (and
// the call in flight goes untold), drop the interrupted note, tell a session
// cut off its calls in the words of one saved where it ends, or continue the
// session after a second loss, and a case goes red.
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

	carriedOn := func(basis runner.SessionBasis, calls []runner.BridgedCall, losses int) runner.AgentRunRequest {
		t.Helper()
		relauncher := &probingLauncher{basis: basis}
		lost := runner.Lost{Why: "the engine it ran under restarted, which killed it", BoxKept: true,
			Losses: losses, Limit: 2}
		collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
			resume: &runner.Resume{State: parked.State, Bridged: calls, Lost: &lost}})
		if _, _, err := collector.Resume(t.Context(), nil); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		// ASKED ONCE WHATEVER THE LOSSES: a round lost a second time continues
		// nothing, and still names the session the probe read.
		if relauncher.asked() != 1 {
			t.Errorf("the launcher was asked %d times, want once", relauncher.asked())
		}
		return relauncher.last(t)
	}

	// SAVED ONCE ITS FIRST CALL WAS ANSWERED: the two after it may not be in
	// the session, and are told as calls it may already show.
	basis := keptBasis
	basis.Interrupted, basis.Through = true, 1
	first := carriedOn(basis, bridged, 1)
	if first.Session == nil {
		t.Fatal("a lost job's round continues no kept session")
	}
	brief := first.Session.Brief
	for _, want := range []string{"## Your previous segment was cut off", prompts.InterruptedSession,
		"## Calls this run made that the session above may not show", "jira_create", "slack_history"} {
		if !strings.Contains(brief, want) {
			t.Errorf("the carried-on session is not told %q:\n%s", want, brief)
		}
	}
	if strings.Contains(brief, "slack_post") {
		t.Errorf("the carried-on session is handed a call it holds:\n%s", brief)
	}

	// PLACED AT THE ROUND'S START, as a capture cut off is where its launch
	// has landed none: every call of the round is told, those the session
	// holds among them, as calls it may already show and each one call —
	// never as calls it lacks.
	start := keptBasis
	start.Interrupted, start.Through = true, 0
	fromStart := carriedOn(start, bridged, 1)
	if fromStart.Session == nil {
		t.Fatal("a lost job's round placed at its start continues no kept session")
	}
	for _, want := range []string{"## Calls this run made that the session above may not show",
		"ONE call, made once", "slack_post", "jira_create", "slack_history"} {
		if !strings.Contains(fromStart.Session.Brief, want) {
			t.Errorf("placed at the round's start, the carried-on session is not told %q:\n%s", want,
				fromStart.Session.Brief)
		}
	}
	if strings.Contains(fromStart.Session.Brief, "so they are not in it") {
		t.Errorf("placed at the round's start, the carried-on session is told calls it may hold are not in "+
			"it:\n%s", fromStart.Session.Brief)
	}

	// SAVED ONCE ITS SECOND CALL WAS ANSWERED, while its first — a slow one,
	// made before it — was still in flight: the log took the second first and
	// the first only after the save, so the first's outcome is not in the
	// session either.
	slow := slices.Clone(bridged)
	slow[0].Appended, slow[1].Appended, slow[2].Appended = 2, 1, 3
	inFlight := keptBasis
	inFlight.Interrupted, inFlight.Through = true, 1
	if brief := carriedOn(inFlight, slow, 1).Session.Brief; !strings.Contains(brief, "slack_post") ||
		!strings.Contains(brief, "slack_history") || strings.Contains(brief, "jira_create") {
		t.Errorf("saved with its first call in flight, the session is told:\n%s\nwant the call in flight "+
			"and the one after the save, never the one it holds", brief)
	}

	if second := carriedOn(basis, bridged, 2); second.Session != nil {
		t.Errorf("a round lost a second time continues %+v, want a session of its own", second.Session)
	}
}

// A KEPT SESSION CUT OFF IS TOLD AGAIN EVERY NOTE THE TURN HAS READ. A capture
// whose harness was cut off may lack its job's last take of the notes, recorded
// read before its answer reached the harness, and is not known to hold its
// job's brief, which told the notes the session it continued lacked — an
// earlier job's among them. No record says which those were, so a relaunch
// that continues it — a person's answer or a loss alike — tells it, after the
// calls it may not show and before the note waiting for it, every note the turn
// has read, once each, in the order read, under a header that counts them and
// says a note shown both above and there is one note — a second copy of one,
// unheaded, would read as a note of its own; the note waiting follows, once.
// Whatever cursor its capture states: none, as this build stamps a capture cut
// off, or the notes before its job's last take, as an older build stamped one.
// A capture whose harness ended its own run holds every note read up to its
// cursor, and is told those read after it, and only those, as before.
//
// Mutation: tell a session cut off only the notes past its cursor, head them
// only where its capture states a cursor, tell them without their header, after
// the note waiting or a second time with no header, or tell a whole session its
// notes as ones it may already show or none of those read after its cursor, and
// a case goes red.
func TestAKeptSessionCutOffIsToldAgainEveryNoteTheTurnRead(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	read := func(id, text string, order int, ph types.Phase, boundary types.SteerBoundary, job string) steer.Read {
		return steer.Read{Note: steer.Note{ID: id, Text: text, By: "founder"},
			Reading: steer.Reading{Phase: ph, Iteration: 1, Round: 1, Boundary: boundary, Launch: job, Order: order}}
	}
	steered := []steer.Read{
		read("earlier", "an earlier job's note", 1, types.PhaseExecute, types.SteerBoundaryToolBatch, "job-0"),
		read("brief", "the note the job's brief carried", 2, types.PhaseExecute, types.SteerBoundaryBrief, "job-1"),
		read("take", "the note the job's last take read", 3, types.PhaseExecute, types.SteerBoundaryToolBatch,
			"job-1"),
		read("review", "the note a review read after the save", 4, types.PhaseReview, types.SteerBoundaryRound, ""),
	}
	told := make([]string, 0, len(steered))
	for _, r := range steered {
		told = append(told, prompts.SteerMessage(r.By, r.Text))
	}
	answer := func(r *runner.Resume) {
		r.Asked = &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder",
			SessionID: "s-1", BoxKept: true}
	}
	loss := func(r *runner.Resume) {
		r.Lost = &runner.Lost{Why: "the engine it ran under restarted, which killed it", BoxKept: true,
			Losses: 1, Limit: 2}
	}
	type notesCase struct {
		basis     runner.SessionBasis
		carry     func(*runner.Resume)
		told, not []string
		// again says the notes are told as ones the session may already show.
		again bool
	}
	// WHOLE, holding every note read when it was packed.
	whole := keptBasis
	whole.NoteCursor = 3
	cases := map[string]notesCase{
		"a person's answer, its kept session whole": {basis: whole, carry: answer, told: told[3:], not: told[:3]},
	}
	// CUT OFF, every call of the round after its save point, whatever cursor it
	// states: none, as this build stamps a capture cut off (internal/sandbox's
	// notesHeld), and the notes before its job's last take, as an older build
	// stamped one.
	for stamped, cursor := range map[string]int{"as this build stamps it": 0, "as an older build stamped it": 2} {
		cutOff := keptBasis
		cutOff.Interrupted, cutOff.Through, cutOff.NoteCursor = true, 0, cursor
		cases["a person's answer, its kept session cut off "+stamped] = notesCase{basis: cutOff, carry: answer,
			told: told, again: true}
		cases["a loss, its kept session cut off "+stamped] = notesCase{basis: cutOff, carry: loss, told: told,
			again: true}
	}
	waiting := steer.Note{ID: "waiting", Text: "the note still waiting", By: "founder"}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			box := steer.New()
			box.Offer(waiting)
			relauncher := &probingLauncher{basis: tc.basis}
			resume := &runner.Resume{State: parked.State, Steered: steered, Bridged: []runner.BridgedCall{
				numbered("jira_create", map[string]any{"summary": "the job's issue"}, 1, 1)}}
			tc.carry(resume)
			collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
				steer: box, resume: resume})
			if _, _, err := collector.Resume(t.Context(), nil); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			again := relauncher.last(t)
			if again.Session == nil || again.Session.Basis.SessionID != "s-1" {
				t.Fatalf("the relaunch continues %+v, want the turn's kept session", again.Session)
			}
			brief := again.Session.Brief
			waitingMsg := prompts.SteerMessage(waiting.By, waiting.Text)
			last := strings.Index(brief, waitingMsg)
			if last < 0 || strings.Count(brief, waitingMsg) != 1 {
				t.Fatalf("the session is not told the note waiting for it once:\n%s", brief)
			}
			for _, not := range tc.not {
				if strings.Contains(brief, not) {
					t.Errorf("a session whose harness ended its own run is told %q again:\n%s", not, brief)
				}
			}
			header := strings.Index(brief, notesAgainHeader)
			switch {
			case !tc.again && header >= 0:
				t.Errorf("a session whose harness ended its own run is told its notes as ones it may "+
					"already show:\n%s", brief)
			case tc.again && (header < strings.Index(brief, cutOffHeader) ||
				!strings.Contains(brief[max(header, 0):], "the 4 notes below")):
				t.Errorf("the notes told again are not headed, after the calls it may not show, as the 4 "+
					"notes below the session may not show:\n%s", brief)
			}
			at := header
			for _, want := range tc.told {
				i := strings.Index(brief, want)
				if i < 0 || i < at || i > last || strings.Count(brief, want) != 1 {
					t.Errorf("the session is not told %q once, in the order read and before the note waiting:\n%s",
						want, brief)
				}
				at = i
			}
		})
	}
}

// A KEPT SESSION IS TOLD AGAIN EVERY NOTE FROM A READ ITS HARNESS NEVER HAD. A
// take whose write landed after its answer was lost is marked unanswered on the
// record: the session lacks that note though its harness ended its own run, and
// holds the answered reads after it. So a relaunch that continues it — a
// person's answer or a send-back alike — tells it every note from that read on,
// once each, in the order read, under the header of notes it may already show,
// and none before, which the session holds; the note waiting follows, once. As
// much where the capture's cursor already stops before the read
// (internal/sandbox's notesHeld) as where its mark landed after the capture was
// stamped and the cursor counts it — the mark carried from the record the
// segment resumed from, or landed after that and read by the relaunch's own
// brief. With two reads marked around an answered one, the first stops it. A
// take marked unanswered after the session was saved is told as every read
// after the save is, unheaded. The fresh brief beside it tells every note, as it
// always does.
//
// Mutation: tell only the notes past the capture's cursor where it counts the
// read, stop at the last read marked rather than the first, ignore a mark the
// relaunch's brief read, tell them without their header, tell the notes before
// the read too, or leave the read itself out, and a case goes red.
func TestAKeptSessionIsToldAgainEveryNoteFromAReadItsHarnessNeverHad(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	read := func(id, text string, order int, boundary types.SteerBoundary, unanswered bool) steer.Read {
		return steer.Read{Note: steer.Note{ID: id, Text: text, By: "founder"},
			Reading: steer.Reading{Phase: types.PhaseExecute, Iteration: 1, Round: 1, Boundary: boundary,
				Launch: "job-1", Order: order, Unanswered: unanswered}}
	}
	steered := []steer.Read{
		read("brief", "the note the job's brief carried", 1, types.SteerBoundaryBrief, false),
		read("lost", "the note whose take's answer was lost", 2, types.SteerBoundaryToolBatch, true),
		read("after", "the note an answered take read after it", 3, types.SteerBoundaryToolBatch, false),
	}
	msg := func(r steer.Read) string { return prompts.SteerMessage(r.By, r.Text) }
	waiting := steer.Note{ID: "waiting", Text: "the note still waiting", By: "founder"}
	answer := func(r *runner.Resume) {
		r.Asked = &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder",
			SessionID: "s-1", BoxKept: true}
	}
	unmarked := slices.Clone(steered)
	unmarked[1].Unanswered = false
	twice := append(slices.Clone(steered),
		read("lost-again", "the note a later take lost", 4, types.SteerBoundaryToolBatch, true))
	for name, tc := range map[string]struct {
		cursor    int
		steered   []steer.Read
		told, not []string
		count     string
		// marks is what the relaunch's brief reads the record to mark
		// unanswered now, nil for the reads as they were carried.
		marks []string
	}{
		"two reads marked around an answered one": {cursor: 4, steered: twice,
			told: []string{msg(twice[1]), msg(twice[2]), msg(twice[3])}, not: []string{msg(twice[0])},
			count: "the 3 notes below"},
		"marked after the segment resumed, its brief reading the mark": {cursor: 3, steered: unmarked,
			marks: []string{"lost"}, told: []string{msg(unmarked[1]), msg(unmarked[2])},
			not: []string{msg(unmarked[0])}, count: "the 2 notes below"},
		"its cursor stopped before the read": {cursor: 1, steered: steered,
			told: []string{msg(steered[1]), msg(steered[2])}, not: []string{msg(steered[0])},
			count: "the 2 notes below"},
		"its cursor counting the read, marked after the stamp": {cursor: 3, steered: steered,
			told: []string{msg(steered[1]), msg(steered[2])}, not: []string{msg(steered[0])},
			count: "the 2 notes below"},
		// MARKED AFTER THE SAVE, the session saved holding the brief's note
		// alone: past where the cursor reaches, as every read after the save.
		"a read marked after the session was saved": {cursor: 1, steered: []steer.Read{
			read("before", "the note read before the save", 1, types.SteerBoundaryBrief, false),
			read("after-save", "the note read after the save", 2, types.SteerBoundaryToolBatch, false),
			read("lost-later", "the note a later take lost", 3, types.SteerBoundaryToolBatch, true)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			box := steer.New()
			box.Offer(waiting)
			var source runner.SteerSource = box
			if tc.marks != nil {
				source = markingBox{Box: box, unanswered: tc.marks}
			}
			basis := keptBasis
			basis.NoteCursor = tc.cursor
			relauncher := &probingLauncher{basis: basis}
			resume := &runner.Resume{State: parked.State, Steered: tc.steered, Bridged: []runner.BridgedCall{
				numbered("jira_create", map[string]any{"summary": "the job's issue"}, 1, 1)}}
			answer(resume)
			collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
				steer: source, resume: resume})
			if _, _, err := collector.Resume(t.Context(), nil); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			again := relauncher.last(t)
			if again.Session == nil || again.Session.Basis.SessionID != "s-1" {
				t.Fatalf("the relaunch continues %+v, want the turn's kept session", again.Session)
			}
			brief := again.Session.Brief
			waitingMsg := prompts.SteerMessage(waiting.By, waiting.Text)
			last := strings.Index(brief, waitingMsg)
			if last < 0 || strings.Count(brief, waitingMsg) != 1 {
				t.Fatalf("the session is not told the note waiting for it once:\n%s", brief)
			}
			for _, r := range tc.steered {
				if !strings.Contains(again.Brief, msg(r)) {
					t.Errorf("the fresh brief does not carry note %q", r.ID)
				}
			}
			header := strings.Index(brief, notesAgainHeader)
			if tc.count == "" {
				if header >= 0 {
					t.Errorf("a session saved before the read marked unanswered is told its notes as ones it "+
						"may already show:\n%s", brief)
				}
				for _, r := range tc.steered[1:] {
					if strings.Count(brief, msg(r)) != 1 {
						t.Errorf("the session is not told %q, read after it was saved, once:\n%s", r.ID, brief)
					}
				}
				if strings.Contains(brief, msg(tc.steered[0])) {
					t.Errorf("the session is told %q again, which it holds:\n%s", tc.steered[0].ID, brief)
				}
				return
			}
			if header < 0 || !strings.Contains(brief[header:], tc.count) {
				t.Fatalf("the notes from the read its harness never had are not headed as %s the session may "+
					"not show:\n%s", tc.count, brief)
			}
			for _, not := range tc.not {
				if strings.Contains(brief, not) {
					t.Errorf("the session is told %q again, which it holds:\n%s", not, brief)
				}
			}
			at := header
			for _, want := range tc.told {
				i := strings.Index(brief, want)
				if i < 0 || i < at || i > last || strings.Count(brief, want) != 1 {
					t.Errorf("the session is not told %q once, under the header, in the order read and before "+
						"the note waiting:\n%s", want, brief)
				}
				at = i
			}
		})
	}
}

// A RELAUNCH TELLS A NOTE A LOST TAKE RECORDED WHERE THE RECORD READ IT. A take
// whose answer was lost hands its notes over only once its settle is answered
// — at a relaunch's brief, after a later round of the segment read a note
// filed after them, a correction of one among them — while the record numbered
// their read where the take was made, and the segment's own reads reach the
// runner unnumbered. So the brief tells such a note at the record's place among
// the notes the turn has read, ahead of the later round's note, in the session
// the relaunch continues and in the fresh brief beside it alike, and the notes
// the brief is the first to read after them.
//
// Mutation: tell a note the source places among the brief's own notes, or a
// read of the segment where the runner came to hold it, and a case goes red.
func TestARelaunchTellsANoteALostTakeRecordedWhereTheRecordReadIt(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	msg := func(n steer.Note) string { return prompts.SteerMessage(n.By, n.Text) }
	held := steer.Note{ID: "held", Text: "the note the session read in its brief", By: "founder"}
	lost := steer.Note{ID: "lost", Text: "use staging", By: "founder"}
	later := steer.Note{ID: "later", Text: "no, use production instead of staging", By: "founder"}
	waiting := steer.Note{ID: "waiting", Text: "the note still waiting", By: "founder"}
	box := steer.New()
	for _, n := range []steer.Note{lost, waiting} {
		box.Offer(n)
	}
	// THE RECORD READ held first, then lost by a take whose answer was lost,
	// then later by the segment's next round — which reached the runner
	// unnumbered, as every read of the segment does.
	source := placingBox{Box: box, orders: map[string]int{"lost": 2, "later": 3}}
	basis := keptBasis
	basis.NoteCursor = 1
	relauncher := &probingLauncher{basis: basis}
	resume := &runner.Resume{State: parked.State, Steered: []steer.Read{
		{Note: held, Reading: steer.Reading{Phase: types.PhaseExecute, Iteration: 1, Round: 1,
			Boundary: types.SteerBoundaryBrief, Launch: "job-1", Order: 1}},
		{Note: later, Reading: steer.Reading{Phase: types.PhaseReview, Iteration: 1, Round: 2,
			Boundary: types.SteerBoundaryRound}},
	},
		Asked: &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder", SessionID: "s-1",
			BoxKept: true},
		Bridged: []runner.BridgedCall{numbered("jira_create", map[string]any{"summary": "the job's issue"}, 1, 1)}}
	collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
		steer: source, resume: resume})
	if _, _, err := collector.Resume(t.Context(), nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	again := relauncher.last(t)
	if again.Session == nil {
		t.Fatal("the relaunch continues no kept session")
	}
	for name, tc := range map[string]struct {
		brief string
		order []steer.Note
		not   []steer.Note
	}{
		"the session it continues": {brief: again.Session.Brief, order: []steer.Note{lost, later, waiting},
			not: []steer.Note{held}},
		"the fresh brief beside it": {brief: again.Brief, order: []steer.Note{held, lost, later, waiting}},
	} {
		at := -1
		for _, n := range tc.order {
			i := strings.Index(tc.brief, msg(n))
			if i < 0 || i < at || strings.Count(tc.brief, msg(n)) != 1 {
				t.Errorf("%s is not told %q once, in the order the record read the notes:\n%s", name, n.ID,
					tc.brief)
			}
			at = i
		}
		for _, n := range tc.not {
			if strings.Contains(tc.brief, msg(n)) {
				t.Errorf("%s is told %q, which the session holds:\n%s", name, n.ID, tc.brief)
			}
		}
	}
}

// A SESSION IS TOLD A NOTE ITS HARNESS WAS SERVED AS ONE IT MAY ALREADY SHOW. A
// note the run's bridge served an OpenCode harness is recorded read only once
// the harness confirms delivering it, so a session continued from that job may
// hold one the record says waits, or that only a review read since. A relaunch
// that continues it tells such a note under the header of notes it may already
// show, never as one it had not read: still waiting, among the notes the brief
// is the first to read — the header taking every note before it, so the order
// read is kept, and only the notes after it following plainly — and read since
// by a review, among the notes past the session's cursor. The fresh brief
// beside it tells every note plainly, as it always does.
//
// Mutation: tell a served note plainly, take the notes after the last served
// one under the header, or leave a served note a review read since plain, and a
// case goes red.
func TestASessionIsToldANoteItsHarnessWasServedAsOneItMayAlreadyShow(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	read := func(id, text string, order int, ph types.Phase, boundary types.SteerBoundary) steer.Read {
		return steer.Read{Note: steer.Note{ID: id, Text: text, By: "founder"},
			Reading: steer.Reading{Phase: ph, Iteration: 1, Round: 1, Boundary: boundary, Order: order}}
	}
	held := read("held", "the note the session read in its brief", 1, types.PhaseExecute, types.SteerBoundaryBrief)
	reviewed := read("reviewed", "the note a review read after the save", 2, types.PhaseReview,
		types.SteerBoundaryRound)
	note := func(id, text string) steer.Note { return steer.Note{ID: id, Text: text, By: "founder"} }
	early := note("early", "the note waiting, never served")
	served := note("served", "the note served to the job's harness, its confirmation lost")
	late := note("late", "the note filed after it")
	msg := func(by, text string) string { return prompts.SteerMessage(by, text) }
	for name, tc := range map[string]struct {
		steered []steer.Read
		served  []string
		waiting []steer.Note
		// again is what the header holds, in order, and plain what follows
		// it; none is the header absent.
		again, plain []string
	}{
		"a served note still waiting": {steered: []steer.Read{held}, served: []string{"served"},
			waiting: []steer.Note{early, served, late},
			again:   []string{msg(early.By, early.Text), msg(served.By, served.Text)},
			plain:   []string{msg(late.By, late.Text)}},
		"a served note a review read since": {steered: []steer.Read{held, reviewed}, served: []string{"reviewed"},
			waiting: []steer.Note{late},
			again:   []string{msg(reviewed.By, reviewed.Text)}, plain: []string{msg(late.By, late.Text)}},
		"no note served": {steered: []steer.Read{held, reviewed}, waiting: []steer.Note{early, late},
			plain: []string{msg(reviewed.By, reviewed.Text), msg(early.By, early.Text), msg(late.By, late.Text)}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			box := steer.New()
			for _, n := range tc.waiting {
				box.Offer(n)
			}
			basis := keptBasis
			basis.NoteCursor = 1
			relauncher := &probingLauncher{basis: basis}
			resume := &runner.Resume{State: parked.State, Steered: tc.steered, Served: tc.served,
				Asked: &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder",
					SessionID: "s-1", BoxKept: true},
				Bridged: []runner.BridgedCall{numbered("jira_create", map[string]any{"summary": "the job's issue"},
					1, 1)}}
			collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
				steer: box, resume: resume})
			if _, _, err := collector.Resume(t.Context(), nil); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			again := relauncher.last(t)
			if again.Session == nil {
				t.Fatal("the relaunch continues no kept session")
			}
			brief := again.Session.Brief
			if strings.Contains(brief, msg(held.By, held.Text)) {
				t.Errorf("the session is told the note it holds:\n%s", brief)
			}
			header := strings.Index(brief, notesAgainHeader)
			at := header
			switch {
			case len(tc.again) == 0 && header >= 0:
				t.Errorf("a session no note was served to is told notes it may already show:\n%s", brief)
			case len(tc.again) > 0 && (header < 0 || !strings.Contains(brief[header:],
				map[int]string{1: "the note below", 2: "the 2 notes below"}[len(tc.again)])):
				t.Fatalf("the notes it may already show are not headed and counted, %d of them:\n%s",
					len(tc.again), brief)
			}
			for _, want := range append(slices.Clone(tc.again), tc.plain...) {
				i := strings.Index(brief, want)
				if i < 0 || i < at || strings.Count(brief, want) != 1 {
					t.Errorf("the session is not told %q once, in the order read:\n%s", want, brief)
				}
				at = i
			}
			for _, w := range tc.waiting {
				if !strings.Contains(again.Brief, msg(w.By, w.Text)) {
					t.Errorf("the fresh brief does not carry note %q", w.ID)
				}
			}
			if strings.Contains(again.Brief, notesAgainHeader) {
				t.Errorf("the fresh brief tells its notes as ones it may already show:\n%s", again.Brief)
			}
		})
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

// A SESSION CONTINUED IN A NEW BOX IS TOLD WHAT THAT BOX HOLDS. Where the
// run's surface offers Gitlet's checkout, a new box holds none of the
// repositories the session checked out of it — nothing is cloned at launch —
// so the continuation tells it to check each out again with that tool, the
// same branch and access and no base, and calls a fresh checkout only what
// the box's setup clones; a run offered no checkout, in a company that hosts
// no code, is told the box is on a fresh checkout, as before. The form for
// the box the session was last worked in says neither.
//
// Mutations: hand the new-box form no checkout whatever the surface offers →
// Gitlet's case, red; keep "on a FRESH checkout of the repository"
// beside a checkout → red; write the checkout's paragraph for a run offered
// none → the other case, red.
func TestASessionContinuedInANewBoxIsToldWhatThatBoxHolds(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
	before := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"looking into it"}`)}})
	parked, _ := launched.Suspended()
	parked.State.ActiveTools = req.Surface.Active()
	for name, tc := range map[string]struct {
		checkout tools.Callable
		want     []string
		not      []string
	}{
		"in a company that hosts code": {
			checkout: stubTool{name: "code_checkout"},
			want: []string{"you are in a new one. Anything it had not committed",
				"is not in this box until you check it out again with `code_checkout`, naming the same " +
					"branch and access and no `base`: it is cloned at that branch's head",
				"A repository the box's setup clones is a FRESH checkout."},
			not: []string{"on a FRESH checkout of the repository"},
		},
		"in a company that hosts none": {
			want: []string{"you are in a new one, on a FRESH checkout of the repository."},
			not:  []string{"Gitlet", "check it out again"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			relauncher := &probingLauncher{basis: keptBasis}
			resume := &runner.Resume{State: parked.State, Bridged: before,
				Asked: &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder",
					Branch: "seats/cto/wk-1", SessionID: "s-1", BoxKept: true}}
			collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
				resume: resume, checkout: tc.checkout})
			if _, _, err := collector.Resume(t.Context(), nil); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			again := relauncher.last(t)
			if again.Session == nil {
				t.Fatal("the answered run's relaunch continues no session, want the kept one")
			}
			_, box, found := strings.Cut(again.Session.Replaced, "## This run is in a new box")
			if !found {
				t.Fatalf("the new-box continuation does not say it is in a new box:\n%s", again.Session.Replaced)
			}
			for _, want := range tc.want {
				if !strings.Contains(box, want) {
					t.Errorf("the new-box continuation does not say %q:\n%s", want, box)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(box, not) {
					t.Errorf("the new-box continuation says %q:\n%s", not, box)
				}
			}
			if strings.Contains(again.Session.Brief, "## This run is in a new box") ||
				strings.Contains(again.Session.Brief, "check it out again") {
				t.Errorf("the continuation for the session's own box speaks of a new one:\n%s",
					again.Session.Brief)
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

// AN IN-PLACE CONTINUATION IS HELD TO THE LARGER OF ITS COPY'S AND ITS CAPTURE'S
// MEASURE. A person's answer carries the asking run on in the session its box
// holds, whose whole context it sends before the answer. Two figures say what
// that context is: the job's newest capture's measure of its archive, and the
// job's record's of the copy itself, which the read that collected the job took
// off its run's own account of its requests ([runner.Resume.ContextTokens]),
// and which a company that keeps no session has alone. The copy is the capture
// or grew past it, so the launch is held to the larger of the two.
//
// Mutation: hand the launch the capture's measure alone, and a copy that grew
// past its capture, or one no capture measured, is held to less than it sends.
func TestAnInPlaceContinuationIsHeldToTheLargerOfItsCopysAndItsCapturesMeasure(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ capture, copied, want int64 }{
		"the copy grew past its capture": {capture: 150000, copied: 180000, want: 180000},
		"the capture measured more":      {capture: 150000, copied: 90000, want: 150000},
		"no capture measured the copy":   {copied: 120000, want: 120000},
		"neither measured it":            {},
		"no record measured the copy":    {capture: 150000, want: 150000},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
			launched, req := launch(t, entries, buildOpts{}, 1, "", nil)
			before := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"looking into it"}`)}})
			parked, _ := launched.Suspended()
			parked.State.ActiveTools = req.Surface.Active()
			relauncher := &probingLauncher{basis: runner.SessionBasis{
				Why: "the newest capture of the job was not kept: refused", ContextTokens: tc.capture}}
			asked := &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder",
				Branch: "wip/t1", SessionID: "sess-ask", BoxKept: true}
			collector, _ := buildWith(t, entries, buildOpts{calls: turnctx.NewCallLog(), launcher: relauncher,
				resume: &runner.Resume{State: parked.State, Asked: asked, Bridged: before, ContextTokens: tc.copied}})
			if _, _, err := collector.Resume(t.Context(), nil); err != nil {
				t.Fatalf("Resume: %v", err)
			}
			again := relauncher.last(t)
			if again.Continue == nil || again.Continue.SessionID != "sess-ask" ||
				again.Continue.ContextTokens != tc.want {
				t.Fatalf("the relaunch continues in place %+v, want the asking run's own session carrying the %d "+
					"tokens it sends first", again.Continue, tc.want)
			}
		})
	}
}

// guarded is a seat whose surface carries one required skill covering
// slack_post, relaunching its executor in round 2 of its turn — with what its
// launcher says the relaunch continues, the job before's parked state and
// calls, whether that job's box said it took up the session its launch chose
// (its record's word, [runner.Resume.Continued]), the turn's closed rounds, how
// the launch decides, and what the relaunched job's own box says it started.
// carry, where set, makes it a relaunch that carries the job's round on — a
// person's answer, or the loss of its job — through the resume that would have
// collected it ([runner.Runner.Resume]); nil relaunches as a review's
// send-back does, in a segment that collected the job.
type guarded struct {
	basis     runner.SessionBasis
	state     execstate.State
	bridged   []runner.BridgedCall
	continued bool
	history   []ledger.Iteration
	decides   func(runner.AgentRunRequest) runner.SessionDecision
	says      *bool
	carry     func(*runner.Resume)
	// during is what the run does once its launch has decided and before the
	// launch returns ([playLauncher.during]).
	during func(runner.AgentRunRequest)
}

// loadSkill is a call that loaded the seat's one required skill, the seq'th
// the bridge admitted and the log took.
func loadSkill(seq int) runner.BridgedCall {
	return runner.BridgedCall{Call: ledger.Call{Name: builtin.LoadToolSkillTool,
		Args: map[string]any{"key": "chat-conventions"}, Result: "Always thread your reply."}, Seq: seq,
		Appended: seq}
}

// relaunch builds g's seat and relaunches its executor, returning what the
// launch was handed.
func (g guarded) relaunch(t *testing.T) runner.AgentRunRequest {
	t.Helper()
	_, req := g.relaunched(t)
	return req
}

// relaunched is [guarded.relaunch] with the runner that launched.
func (g guarded) relaunched(t *testing.T) (*runner.Runner, runner.AgentRunRequest) {
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
	launcher.decides, launcher.says, launcher.during = g.decides, g.says, g.during
	state := g.state
	state.Version, state.Round = execstate.Version, 1
	resume := &runner.Resume{State: state, Bridged: g.bridged, Continued: g.continued}
	if g.carry != nil {
		g.carry(resume)
	}
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: organization, Role: role},
		Registry: reg, Models: models, Skills: registry,
		Launcher: launcher, Task: "post the summary",
		Resume: resume,
		Turn: runner.Turn{RunID: "t-guard", WorkKey: "wk-t-guard", AgentID: "agent-1",
			Context: &turnctx.Turn{RunID: "t-guard", WorkKey: "wk-t-guard", Seat: role, Org: organization}},
		Sessions: sessions,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	if g.carry != nil {
		if _, _, err := r.Resume(t.Context(), g.history); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		return r, launcher.last(t)
	}
	if _, _, err := r.Execute(t.Context(), 2, "thread the reply", g.history); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	return r, launcher.last(t)
}

// A LAUNCH RECORDS THE SKILLS IT ARMED, NEVER A LOAD ITS RUN MADE AS IT STARTED.
// The run starts inside its launch, on the guard the launch armed, so its
// harness can report a skill load over the bridge before the launch returns.
// That load is a call of the job's own, placed past its BridgeFrom, which every
// rebuild of its guard replays only where the session it continues holds it —
// never where the job's own copy in its box is continued after its harness was
// cut off, or a capture of it saved before the load. So the job's parked state
// records the skills its launch armed as it armed them, before the run started,
// on a first launch and on a relaunch alike: none for a session of the run's
// own, and the ones the session it continues held for a capture its launch
// takes up.
//
// Mutation: record the guard as it stands once the launch returns, at either
// launch, and the load is recorded as one the launch armed; record the guard
// before the launch arms it, and the continued capture's skill is not.
func TestALaunchRecordsTheSkillsItArmedNeverALoadItsRunMadeAsItStarted(t *testing.T) {
	t.Parallel()
	inBox := runner.SessionBasis{Why: "the newest capture of the job was not kept: refused"}
	active := guarded{basis: inBox}.relaunch(t).Surface.Active()
	rotated := func(r *runner.Resume) { r.Rotated = &runner.Rotated{SessionID: "sess-1", BoxKept: true} }
	for name, tc := range map[string]struct {
		g     guarded
		loads bool
		want  []string
	}{
		"a first launch, its run loading a skill as it starts": {loads: true},
		"a relaunch carrying the round on in its job's box, its run loading a skill as it starts": {
			g: guarded{basis: inBox, state: execstate.State{ActiveTools: active}, carry: rotated}, loads: true},
		"a relaunch continuing a capture whose session held a skill": {g: guarded{basis: keptBasis,
			continued: true, state: execstate.State{ActiveTools: active, LoadedSkills: []string{"chat-conventions"}},
			carry: rotated}, want: []string{"chat-conventions"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// THE LOAD, made as the run starts: once its launch has decided,
			// before the launch returns.
			var made []runner.BridgedCall
			g := tc.g
			if tc.loads {
				g.during = func(req runner.AgentRunRequest) {
					made = play(t, req, run{calls: []llm.ToolCall{
						call(t, builtin.LoadToolSkillTool, `{"key":"chat-conventions"}`)}})
				}
			}
			r, _ := g.relaunched(t)
			if tc.loads && (len(made) != 1 || made[0].Failed) {
				t.Fatalf("the run's load as it started = %+v, want it made", made)
			}
			parked, ok := r.Suspended()
			if !ok {
				t.Fatal("the launch recorded no state")
			}
			if got := parked.State.LoadedSkills; !slices.Equal(got, tc.want) && len(got)+len(tc.want) > 0 {
				t.Errorf("the parked state records %v as the skills its launch armed, want %v: a load the "+
					"run made as it started is one of the job's own calls", got, tc.want)
			}
		})
	}
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
// brief, the prompt the run did read, published off the path of the request
// that said it, on the runner's own goroutine, which each case waits for
// ([runner.Runner.Sent]) before it counts. A box that took the session up
// publishes nothing more.
//
// Mutation: publish nothing on the box's word, or publish on either word, and a
// case goes red, whichever order the goroutine runs in.
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
			// THE FALL BACK'S FRAME GOES OUT ON ITS OWN, off the request that
			// said it, so it is waited for — in both cases, so a frame a box
			// that took the session up should never have sent is counted too.
			collector.Sent()
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

// baseBasis is the session a conversation's last turn left, as a launcher
// answers it for the turn's first launch: turn t-0's, ended at nine, last
// worked under a home whose path carries a marker, its harness cut off when it
// was saved.
var baseBasis = runner.SessionBasis{SessionID: "s-base", Harness: "claude-code",
	Base: &runner.BaseBasis{Turn: "t-0", Home: "/boxes/old/home [untrusted:x]", Interrupted: true,
		ContextTokens: 40000, EndedAt: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}}

// A CONVERSATION'S NEXT TURN IS TOLD WHERE IT BEGINS. Its first launch asks
// which session it continues — every launch does, a first one too — and is
// answered the session the conversation's last turn left. That session holds
// another turn, so the run is handed a lead before the turn's own brief: where
// the session's turn ended and this one begins, that what it read may be stale
// and what it set up is set up anew, that its box is a new one on a fresh
// checkout whose old paths were the other box's, and that its harness was cut
// off. Then the turn's whole fresh brief, the account of how the conversation's
// earlier turns ended among it, which the session never read. Continued only in
// a box of its own, so it carries no same-box brief; and the engine's record of
// that session is defused under the turn's token.
//
// Mutation: probe only a relaunch, hand the same-turn lead, drop the fresh
// brief or the endings, or leave a marker in the home, and a check goes red.
func TestAConversationsNextTurnIsToldWhereItBegins(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	endings := "## How this conversation's earlier turns ended\n\n### Turn t-0\nIt completed."
	launcher := &probingLauncher{basis: baseBasis}
	first, _ := buildWith(t, entries, buildOpts{launcher: launcher, endings: endings,
		calls: turnctx.NewCallLog()})
	work, _, err := first.Execute(t.Context(), 1, "", nil)
	if err != nil || !work.Suspended {
		t.Fatalf("the first launch: %+v, %v", work, err)
	}
	if launcher.asked() != 1 {
		t.Fatalf("the first launch asked %d times which session it continues, want once", launcher.asked())
	}
	req := launcher.last(t)
	s := req.Session
	if s == nil || !s.Basis.ContinuesBase() || s.Brief != "" {
		t.Fatalf("the first launch continues %+v, want the conversation's session in a box of its own", s)
	}
	for _, said := range []string{
		"## A new turn of this conversation begins here", "through turn `t-0`",
		"which ended at 2026-10-01T09:00:00Z", "may be stale", "set up anew for this turn",
		"FRESH checkout", "/boxes/old/home", prompts.InterruptedSession,
	} {
		if !strings.Contains(s.Replaced, said) {
			t.Errorf("the lead does not say %q:\n%s", said, s.Replaced)
		}
	}
	if !strings.HasPrefix(s.Replaced, "## A new turn of this conversation begins here") ||
		!strings.HasSuffix(s.Replaced, strings.TrimSpace(req.Brief)) {
		t.Errorf("the continuation is not the lead and then the turn's fresh brief whole:\n%s", s.Replaced)
	}
	if !strings.Contains(req.Brief, endings) || !strings.Contains(s.Replaced, endings) {
		t.Errorf("the run is not told how the conversation's earlier turns ended:\n%s", s.Replaced)
	}
	if strings.Contains(s.Replaced, "[untrusted:x]") || strings.Contains(s.Replaced, "same session") {
		t.Errorf("the lead carries a live marker or the same-turn lead:\n%s", s.Replaced)
	}
}

// A SCHEDULED FIRE IS TOLD WHERE IT BEGINS, IN ITS SCHEDULE'S WORDS. A fire's
// conversation is its schedule's, so its first launch continues the session
// the schedule's last fire left, and the lead says so as a fire: the schedule
// fired again, the session above is as the schedule's fires left it, what they
// set up is set up anew for this fire, and this fire is in a new box. Its lead
// never says a message woke it, and a conversation's turn is never told it is
// a fire ([TestAConversationsNextTurnIsToldWhereItBegins]).
//
// Mutation: lead a fire in a conversation's words, or decide the words by
// anything but the turn's conversation, and a check goes red.
func TestAScheduledFireIsToldWhereItBegins(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	launcher := &probingLauncher{basis: baseBasis}
	fire, _ := buildWith(t, entries, buildOpts{launcher: launcher, calls: turnctx.NewCallLog(),
		conversation: notify.ScheduleConversation(types.ScheduleScopeRole, "a-1", "health-check")})
	work, _, err := fire.Execute(t.Context(), 1, "", nil)
	if err != nil || !work.Suspended {
		t.Fatalf("the fire's first launch: %+v, %v", work, err)
	}
	req := launcher.last(t)
	s := req.Session
	if s == nil || !s.Basis.ContinuesBase() {
		t.Fatalf("the fire's first launch continues %+v, want the schedule's last fire's session", s)
	}
	lead := strings.TrimSuffix(s.Replaced, strings.TrimSpace(req.Brief))
	for _, said := range []string{
		"## A new fire of this schedule begins here", "as your fires left it", "through fire `t-0`",
		"The schedule has fired again", "the task below is this fire's", "set up anew for this fire",
		"## This fire is in a new box",
	} {
		if !strings.Contains(lead, said) {
			t.Errorf("the fire's lead does not say %q:\n%s", said, lead)
		}
	}
	for _, unsaid := range []string{"A new message has woken you", "of this conversation", "this turn"} {
		if strings.Contains(lead, unsaid) {
			t.Errorf("the fire's lead says %q, a conversation's words:\n%s", unsaid, lead)
		}
	}
}

// A CONVERSATION'S NEXT TURN IS TOLD WHAT ITS NEW BOX HOLDS. The session its
// last turn left is continued only in a box of its own, and on
// Gitlet nothing is cloned at launch: a run offered Gitlet's checkout
// is told that a repository the session checked out of it is in no box until it
// is checked out again, and how — at its branch's head — never that a fresh
// checkout is in front of it. A run offered none is told of a fresh checkout,
// as before.
//
// Mutations: hand the next turn's lead no checkout whatever the surface offers
// → Gitlet's case, red; keep "on a FRESH checkout of the repository"
// beside a checkout → red; write the checkout's paragraph for a run offered
// none → the other case, red.
func TestAConversationsNextTurnIsToldWhatItsNewBoxHolds(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	for name, tc := range map[string]struct {
		checkout tools.Callable
		want     []string
		not      []string
	}{
		"in a company that hosts code": {
			checkout: stubTool{name: "code_checkout"},
			want: []string{"You are in a new box: anything the session above had not committed",
				"is not in this box until you check it out again with `code_checkout`, naming the same " +
					"branch and access and no `base`: it is cloned at that branch's head",
				"A repository the box's setup clones is a FRESH checkout."},
			not: []string{"on a FRESH checkout of the repository"},
		},
		"in a company that hosts none": {
			want: []string{"You are in a new box, on a FRESH checkout of the repository: anything"},
			not:  []string{"Gitlet", "check it out again"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			launcher := &probingLauncher{basis: baseBasis}
			first, _ := buildWith(t, entries, buildOpts{launcher: launcher, calls: turnctx.NewCallLog(),
				checkout: tc.checkout})
			if work, _, err := first.Execute(t.Context(), 1, "", nil); err != nil || !work.Suspended {
				t.Fatalf("the first launch: %+v, %v", work, err)
			}
			s := launcher.last(t).Session
			if s == nil || !s.Basis.ContinuesBase() {
				t.Fatalf("the first launch continues %+v, want the conversation's session", s)
			}
			// THE LEAD'S NEW-BOX PART ALONE: the turn's fresh brief follows the
			// lead, and what it says is not the box's.
			_, box, found := strings.Cut(s.Replaced, "## This turn is in a new box")
			if !found {
				t.Fatalf("the next turn's lead does not say it is in a new box:\n%s", s.Replaced)
			}
			box, _, found = strings.Cut(box, "Absolute paths in the session above")
			if !found {
				t.Fatalf("the next turn's lead does not say whose its old paths were:\n%s", s.Replaced)
			}
			for _, want := range tc.want {
				if !strings.Contains(box, want) {
					t.Errorf("the next turn's lead does not say %q:\n%s", want, box)
				}
			}
			for _, not := range tc.not {
				if strings.Contains(box, not) {
					t.Errorf("the next turn's lead says %q:\n%s", not, box)
				}
			}
		})
	}
}

// A BASE CONTINUATION ARMS AN EMPTY GUARD, whatever the parked state holds. The
// session the conversation's last turn left loaded skills in another turn,
// which this turn's guard never armed and its lead says were set up anew: so
// once the launch decides its run takes that session up, the guard holds
// nothing, and a skill-covered call is refused until the skill is loaded again
// — the safe direction. That holds where it matters most: a round a person's
// answer or a lost job carries on, the turn having kept no session of its own,
// continues the session its first launch was handed, while the job it follows
// left a parked state whose box said it took that session up and whose launch
// armed the guard with what it loaded there — loads in a session the relaunch
// restores afresh, which read none of them. Such a run is handed the lead, then
// the carried round's reseed, which says what the round's jobs did. A relaunch
// that continues the turn's own kept session is armed with what that session
// loaded, the round's carry or a send-back alike.
//
// Mutation: drop the base branch of the launch's decision (sessionDecided), so a
// base continuation is armed from the parked state as a kept one is, and both
// carried rows on the conversation's session go red; hand such a run the
// carry's continuation rather than its reseed, and their brief's check does.
func TestABaseContinuationArmsAnEmptyGuard(t *testing.T) {
	t.Parallel()
	// THE TOOLS THE ROUND'S LAUNCH WAS HANDED, as its parked state records
	// them: what a relaunch carrying the round on rebuilds its surface from.
	active := guarded{basis: runner.SessionBasis{Why: "the turn has kept no capture"}}.relaunch(t).Surface.Active()
	loaded := execstate.State{ActiveTools: active, LoadedSkills: []string{"chat-conventions"}}
	lost := func(r *runner.Resume) {
		r.Lost = &runner.Lost{Why: "its box was lost from the host it ran on", Losses: 1, Limit: 2}
	}
	answered := func(r *runner.Resume) {
		r.Asked = &runner.Asked{Question: "which branch?", Answer: "use main", By: "founder"}
	}
	for name, tc := range map[string]struct {
		g       guarded
		told    string
		refused bool
	}{
		"a send-back, on the conversation's session": {g: guarded{basis: baseBasis, state: loaded},
			refused: true},
		"a lost job's round, on the conversation's session": {g: guarded{basis: baseBasis, continued: true,
			state: loaded, carry: lost}, told: "## A run before you on this task was cut off", refused: true},
		"an answered job's round, on the conversation's session": {g: guarded{basis: baseBasis, continued: true,
			state: loaded, carry: answered}, told: "use main", refused: true},
		"a send-back, on the turn's own kept session": {g: guarded{basis: keptBasis, continued: true,
			state: loaded}},
		"a lost job's round, on the turn's own kept session": {g: guarded{basis: keptBasis, continued: true,
			state: loaded, carry: lost}},
		"an answered job's round, on the turn's own kept session": {g: guarded{basis: keptBasis,
			continued: true, state: loaded, carry: answered}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			req := tc.g.relaunch(t)
			if s := req.Session; tc.told != "" && (s == nil || !s.Basis.ContinuesBase() ||
				!strings.HasPrefix(s.Replaced, "## A new turn of this conversation begins here") ||
				!strings.HasSuffix(s.Replaced, strings.TrimSpace(req.Brief)) || !strings.Contains(req.Brief, tc.told)) {
				t.Fatalf("the carried round continues %+v, want the conversation's session, handed the lead and "+
					"then the round's reseed, which says %q", s, tc.told)
			}
			bridged := play(t, req, run{calls: []llm.ToolCall{call(t, "slack_post", `{"text":"hi"}`)}})
			if bridged[0].Failed != tc.refused {
				t.Fatalf("slack_post failed %v (%q), want refused %v", bridged[0].Failed, bridged[0].Result,
					tc.refused)
			}
		})
	}
}
