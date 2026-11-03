package sandbox

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// endedHead is swe's ended turn t-head on the conversation chat:room, its kept
// session written by claude-code: the conversation's head wherever a read
// names it so.
func endedHead(now time.Time) executordomain.Session {
	return executordomain.Session{Handle: "swe", Turn: "t-head", ConversationKey: "chat:room",
		Harness: "claude-code", SessionID: "s-head", ArchiveFormat: FakeArchiveFormat,
		Object: objstore.Object{Key: objstore.KeyAt(now), Size: 1}, LaunchSeq: 1, CaptureSeq: 1,
		CapturedAt: now, EndedAt: now.Add(time.Minute),
		Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}}
}

// WHAT A READER IS TOLD OF AN ENDED TURN'S SESSION IS THE NEXT TURN'S OWN
// DECISION. An ended turn's session is continued by its conversation's next
// turn, and only where it is the conversation's HEAD as the read that answered
// the row named it — decided by the decision that turn's first launch takes of
// the head its dispatch reads, of the member the node holding the seat would
// launch it on, asked by no turn: continued in any box where that member's
// runner restores it; not where it refuses it, where the seat has no member,
// or where the company keeps no session; undecided, never a no, where what it
// is launched on cannot be said, or where the read named no head beside the
// row. A row that is not the head names the head the next turn is handed; one
// that names no session says which of three reasons; and a turn that served no
// conversation is continued by no launch at all — and is the one ended row
// about which no launch is said. Only the head asks the node holding the seat
// anything.
//
// Mutation: decide an ended row by a relaunch's decision, as a turn of no
// conversation, by its own turn's target, or by anything but the head the
// read named; read an unknown head or target as a no; ask the holder about a
// row that is not the head, or by the head's own turn; or say a released
// session was released by the budget whatever its end said, and a case goes
// red.
func TestWhatAReaderIsToldOfAnEndedTurnIsTheNextTurnsOwnDecision(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	restores := RelaunchTarget{Harness: "claude-code", Placement: Direct, Node: "node-a", Restorer: fakeRestorer{}}
	refuses := restores
	refuses.Restorer = fakeRestorer{why: "the session was written by claude-code, and this runner drives opencode"}
	keepsNone := restores
	keepsNone.NoKept = NoKeptSession
	for name, tc := range map[string]struct {
		row       func(*executordomain.Session)
		head      string
		target    RelaunchTarget
		by        ContinuedBy
		decided   bool
		continues bool
		why       string
		asks      bool
	}{
		"the head, its runner restoring it": {head: "t-head", target: restores, by: ContinuedByNextTurn,
			decided: true, continues: true, asks: true},
		"the head, its runner refusing it": {head: "t-head", target: refuses, by: ContinuedByNextTurn,
			decided: true, why: "this runner drives opencode", asks: true},
		"the head, the seat having no member": {head: "t-head", by: ContinuedByNextTurn, decided: true,
			target: RelaunchTarget{Node: "node-a", Why: "the company holds no seat swe"},
			why:    "holds no seat swe", asks: true},
		"the head, the company keeping no session": {head: "t-head", target: keepsNone,
			by: ContinuedByNextTurn, decided: true, why: "keeps no executor session", asks: true},
		"the head, what its next turn is launched on not to be said": {head: "t-head", by: ContinuedByNextTurn,
			target: RelaunchTarget{Undecided: true, Why: "every member is sitting out its plan"},
			why:    "sitting out", asks: true},
		"a row the read named no head beside": {by: ContinuedByNextTurn, target: restores,
			why: "did not say which"},
		"a row older than its conversation's head": {head: "t-newer", target: restores,
			by: ContinuedByNextTurn, decided: true, why: "is handed turn t-newer's session"},
		"a turn that served no conversation": {head: "t-head", target: restores, decided: true,
			why: "served no conversation", row: func(s *executordomain.Session) { s.ConversationKey = "" }},
		"a turn that captured nothing": {head: "t-head", target: restores, by: ContinuedByNextTurn,
			decided: true, why: "nothing of this turn's session was captured", row: func(s *executordomain.Session) {
				s.Object, s.CapturedAt, s.LaunchSeq, s.CaptureSeq = objstore.Object{}, time.Time{}, 0, 0
			}},
		"a turn whose newest capture was not kept": {head: "t-head", target: restores, by: ContinuedByNextTurn,
			decided: true, why: "(launch 2, capture 1) was not kept: refused: past the seat's budget",
			row: func(s *executordomain.Session) {
				s.Object = objstore.Object{}
				s.Ending.Unkept = &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1,
					Reason: "refused: past the seat's budget"}
			}},
		"a session the seat's budget released": {head: "t-head", target: restores, by: ContinuedByNextTurn,
			decided: true, why: "budget released", row: func(s *executordomain.Session) {
				s.Object = objstore.Object{}
				// AN UNKEPT CAPTURE NO NEWER THAN THE ONE THE ROW KEPT, as a
				// capture of the very attempt that landed after all leaves
				// it: the end released nothing, and the budget did.
				s.Ending.Unkept = &executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 1, Reason: "unanswered"}
			}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row := endedHead(now)
			if tc.row != nil {
				tc.row(&row)
			}
			got := ReadContinuityOf(row.Kept(), tc.head, nil, tc.target, now)
			switch {
			case got.By != tc.by:
				t.Fatalf("ReadContinuityOf = %+v, want it about %q", got, tc.by)
			case got.Decided != tc.decided || got.Continues != tc.continues:
				t.Fatalf("ReadContinuityOf = %+v, want decided %v, continues %v", got, tc.decided, tc.continues)
			case tc.continues && (got.In != ContinuesInAnyBox || got.Why != ""):
				t.Fatalf("ReadContinuityOf = %+v, want continued in any box, with no reason", got)
			case !tc.continues && (got.In != "" || !strings.Contains(got.Why, tc.why)):
				t.Fatalf("ReadContinuityOf = %+v, want it saying %q", got, tc.why)
			}
			ask, asks := TargetAsk(row.Kept(), tc.head, nil)
			switch {
			case asks != tc.asks:
				t.Fatalf("TargetAsk asks %v (%+v), want %v", asks, ask, tc.asks)
			case asks && ask != (RelaunchAsk{Archive: ArchiveOf(row.Kept())}):
				t.Fatalf("the head asks %+v, want no turn and its own archive", ask)
			}
		})
	}
}

// begunTurn is swe's live turn t-begun, which froze the head t-head as its base
// and whose row on the log holds it, having landed no capture, and whose first
// job was lost with its box before anything of it was kept.
func begunTurn(now time.Time) (executordomain.Session, PendingRun) {
	base := endedHead(now)
	frozen := baseSessionOf(base.Kept())
	row := executordomain.Session{Handle: "swe", Turn: "t-begun", ConversationKey: "chat:room",
		BaseTurn: "t-head"}
	return row, PendingRun{TurnID: "t-begun", AgentHandle: "swe", Status: StatusRunning, LaunchSeq: 1,
		Launch: LaunchRecord{Collected: true}, Lost: &JobLoss{Cause: LossBoxGone},
		Base: &ConversationBase{ReadThrough: 9, Absorbs: []string{"t-head"}, Session: &frozen}}
}

// A RELAUNCH OF A TURN THAT HAS KEPT NOTHING IS SAID TO CONTINUE THE SESSION
// ITS FIRST LAUNCH WAS HANDED, as the relaunch's own question decides it. The
// turn's own row names no session, so it is not continuable, saying why — and
// where the run froze a base, the row still holds it on the log, and the
// member a relaunch is launched on restores it, the row says a relaunch in a
// box of its own continues that session instead. Not where the turn has kept a
// session of its own, nor where its row holds no base (its begin found the
// session gone, or never landed), nor where the company keeps no session or the
// runner refuses the base, nor for a round lost a second time — unless its
// resumed turn collects what the job submitted, whose review's send-back does
// continue it. The holder is asked about the base's archive, which is the one
// such a relaunch takes up, by the turn's own id.
//
// Mutation: drop the base's half, claim it whatever the row holds or the run
// kept, or ask the holder about the row's own empty archive, and a case goes
// red.
func TestARelaunchOfATurnThatKeptNothingIsSaidToContinueItsBase(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	restores := RelaunchTarget{Harness: "claude-code", Placement: Direct, Node: "node-a", Restorer: fakeRestorer{}}
	const said = "a relaunch of the turn in a box of its own continues turn t-head's session instead"
	for name, tc := range map[string]struct {
		row    func(*executordomain.Session)
		run    func(*PendingRun)
		target RelaunchTarget
		base   bool
	}{
		"a turn whose first job was lost before it kept anything": {target: restores, base: true},
		"a turn whose begin holds no base": {target: restores,
			row: func(s *executordomain.Session) { s.BaseTurn = "" }},
		"a turn that froze no base": {target: restores, run: func(r *PendingRun) { r.Base.Session = nil }},
		"a turn that has kept a session of its own": {target: restores, run: func(r *PendingRun) {
			kept := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded, At: now}
			r.Capture, r.Kept = &kept, &kept
		}},
		"a company that keeps no session": {target: RelaunchTarget{Harness: "claude-code", Placement: Direct,
			Node: "node-a", Restorer: fakeRestorer{}, NoKept: NoKeptSession}},
		"a runner that refuses the base": {target: RelaunchTarget{Harness: "claude-code", Placement: Direct,
			Node: "node-a", Restorer: fakeRestorer{why: "older than 2.1.300"}}},
		"a round lost a second time": {target: restores, run: func(r *PendingRun) { r.RoundLosses = 1 }},
		"a round lost a second time, its resumed turn collecting it": {target: restores, base: true,
			run: func(r *PendingRun) {
				r.Status, r.RoundLosses, r.Launch.ResumeCarry = StatusResumed, 1, ResumeCollects
			}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			row, run := begunTurn(now)
			if tc.row != nil {
				tc.row(&row)
			}
			if tc.run != nil {
				tc.run(&run)
			}
			got := ReadContinuityOf(row.Kept(), "t-head", &run, tc.target, now)
			switch {
			case got.By != ContinuedByRelaunch || !got.Decided || got.Continues:
				t.Fatalf("ReadContinuityOf = %+v, want a relaunch decided not to continue the row's own", got)
			case strings.Contains(got.Why, said) != tc.base:
				t.Fatalf("ReadContinuityOf = %+v, want it saying the base is continued: %v", got, tc.base)
			}
		})
	}

	row, run := begunTurn(now)
	ask, asks := TargetAsk(row.Kept(), "t-head", &run)
	if !asks || ask != (RelaunchAsk{Turn: "t-begun", Archive: run.Base.Session.archive()}) {
		t.Errorf("a turn that has kept nothing asks %+v (%v), want its own turn and its base's archive", ask, asks)
	}
}

// A LIVE TURN WHOSE RUN IS OVER OR ENDING IS THE NEXT TURN'S, NOT YET DECIDED.
// No relaunch of a turn whose run is over, or whose ending is decided, follows;
// whether its conversation's next turn continues the session its row names is
// the conversation's head's to say once the turn's end is on the log, which the
// row as read does not show — so it is undecided, never a no, and asks the
// node holding the seat nothing. A turn of no conversation is decided: no
// launch continues it.
//
// Mutation: answer such a row as a relaunch's no, as decided, or ask the holder
// about it, and a case goes red.
func TestALiveTurnWhoseRunIsOverOrEndingIsTheNextTurnsUndecided(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	row := endedHead(now)
	row.Ending, row.EndedAt = nil, time.Time{}
	kept := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded, At: now}
	stopping := &PendingRun{TurnID: "t-head", LaunchSeq: 1, Capture: &kept, Kept: &kept,
		Ending: &RecordedEnding{ID: "ending-1"}}
	target := RelaunchTarget{Harness: "claude-code", Placement: Direct, Node: "node-a", Restorer: fakeRestorer{}}
	for name, run := range map[string]*PendingRun{"its run over": nil, "its ending decided": stopping} {
		got := ReadContinuityOf(row.Kept(), "", run, target, now)
		if got.By != ContinuedByNextTurn || got.Decided || !strings.Contains(got.Why, "not on the executor log") {
			t.Errorf("%s: ReadContinuityOf = %+v, want the next turn's, undecided until its end lands", name, got)
		}
		if ask, asks := TargetAsk(row.Kept(), "", run); asks {
			t.Errorf("%s: a turn no launch of which is decided by a member asks %+v", name, ask)
		}
		keyless := row
		keyless.ConversationKey = ""
		if got := ReadContinuityOf(keyless.Kept(), "", run, target, now); got.By != "" || !got.Decided ||
			!strings.Contains(got.Why, "served no conversation") {
			t.Errorf("%s: a turn of no conversation reads %+v, want decided, continued by no launch", name, got)
		}
	}
}

// WHO CONTINUES A SESSION IS ONE OF TWO, and an answer naming another — one a
// build that knows a third wrote — reads as the value it is.
//
// Mutation: answer true for any value, or drop one from [ContinuedBys], and a
// row goes red.
func TestWhoContinuesASessionIsOneOfTwo(t *testing.T) {
	t.Parallel()
	for by, valid := range map[ContinuedBy]bool{
		ContinuedByRelaunch: true, ContinuedByNextTurn: true, "later": false, "": false,
	} {
		if by.Valid() != valid {
			t.Errorf("%q.Valid() = %v, want %v", by, by.Valid(), valid)
		}
	}
	if len(ContinuedBys) != 2 {
		t.Errorf("ContinuedBys = %v, want the two", ContinuedBys)
	}
}
