package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// executorLogSpy is the executor log's writes as a case reads them: every end
// a turn's ending recorded.
type executorLogSpy struct {
	mu   sync.Mutex
	ends []executordomain.NewEnd
}

func (s *executorLogSpy) Capture(context.Context, executordomain.Call,
	executordomain.NewCapture) (executordomain.CaptureResult, error) {
	return executordomain.CaptureResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}, Recorded: true}, nil
}

func (s *executorLogSpy) End(_ context.Context, _ executordomain.Call, n executordomain.NewEnd) (statelog.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ends = append(s.ends, n)
	return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
}

// endsOf is every end recorded for turnID.
func (s *executorLogSpy) endsOf(turnID string) []executordomain.NewEnd {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []executordomain.NewEnd
	for _, n := range s.ends {
		if n.Turn == turnID {
			out = append(out, n)
		}
	}
	return out
}

// loggingEngine is [spendingEngine] whose sandbox runtime records on spy.
func loggingEngine(t *testing.T) (*Engine, *executorLogSpy) {
	t.Helper()
	e, _ := spendingEngine(t)
	spy := &executorLogSpy{}
	e.executorLog = spy
	return e, spy
}

// A RESUMED TURN THAT ENDS RECORDS ITS END ON THE EXECUTOR LOG, once, with the
// turn's OWN decision — the one its completion carries — and how its reply
// went.
//
// Mutation: drop the end from recordResume, record it for a turn that parked
// again, or take the decision off the reviewer's last word, and the end is
// missing, doubled, or says another decision than the turn's.
func TestAResumedTurnsEndIsRecordedWithItsOwnDecision(t *testing.T) {
	t.Parallel()
	e, spy := loggingEngine(t)
	q := e.backends.Queue.(*memory.Queue)
	model := longAccountModel()
	swapModel(t, e, model)
	rig := rigRuns(t, e, modelPlayer(t, model.pick))
	runToItsEnd(t, e, rig, Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1", WorkSince: time.Now().UTC(),
		Events: []*events.Event{taskWake("task-9", "ENG-9")},
	})
	ends := spy.endsOf("run-1")
	if len(ends) != 1 {
		t.Fatalf("recorded %d ends of the turn, want 1", len(ends))
	}
	var decision string
	for _, ev := range q.History() {
		if done, ok := events.DataAs[*types.AgentTurnCompleted](ev); ok && done.TurnID == "run-1" && !done.Suspended {
			decision = done.Decision
		}
	}
	end := ends[0]
	if end.Handle != "swe" || end.WorkKey != "wk-1" || end.Ending.Decision != decision ||
		!end.Ending.Outcome.Valid() || end.Ending.Outcome == executordomain.OutcomeStopped {
		t.Fatalf("the end is %+v, want the turn's own decision %q", end, decision)
	}
}

// A PARKED TURN A PERSON STOPPED ENDS AS STOPPED ON THE EXECUTOR LOG, by the
// run's ending, which no frame of the turn reaches: who stopped it and why,
// their reason carried as theirs.
//
// Mutation: drop the end from the stop's close, and the next turn on the
// conversation is never told this one was stopped.
func TestAStoppedParkedTurnEndsAsStoppedOnTheExecutorLog(t *testing.T) {
	t.Parallel()
	e, spy := loggingEngine(t)
	rigRuns(t, e, halfwayPlay)
	parkOnRun(t, e)
	pause := janeStops()
	e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})

	ends := spy.endsOf("run-1")
	if len(ends) != 1 {
		t.Fatalf("recorded %d ends of the stopped turn, want 1", len(ends))
	}
	if ending := ends[0].Ending; ending.Outcome != executordomain.OutcomeStopped ||
		ending.StoppedBy != "jane" || ending.StopReason != "wrong repository" {
		t.Fatalf("the end is %+v, want the turn stopped by Jane for her reason", ending)
	}
}

// A LAUNCH STOPPED BEFORE ITS TURN PARKED ENDS ON THE EXECUTOR LOG FROM THE
// FRAME THAT LAUNCHED IT — the run's own ending closes no turn, since none had
// parked on it — once.
//
// Mutation: drop the end from the dispatch path, or record it from the run's
// ending too, and the turn's end is missing or recorded twice.
func TestALaunchStoppedBeforeItsParkEndsOnTheExecutorLogOnce(t *testing.T) {
	t.Parallel()
	e, spy := loggingEngine(t)
	store := &parkStopsFirst{PendingStore: sandbox.NewCoordStore(e.backends.Fleet)}
	rigRunsOn(t, e, store, halfwayPlay)
	store.before = func() {
		pause := janeStops()
		e.applySeatPause(t.Context(), coord.SeatPauseUpdate{Seat: pause.Seat, Pause: &pause})
	}
	if _, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1", WorkSince: time.Now().UTC(),
		Events: []*events.Event{taskWake("task-9", "ENG-9")},
	}); !turn.Stopped(err) {
		t.Fatalf("runTurn = %v, want the turn stopped", err)
	}
	ends := spy.endsOf("run-1")
	if len(ends) != 1 || ends[0].Ending.Outcome != executordomain.OutcomeStopped ||
		ends[0].Ending.StoppedBy != "jane" {
		t.Fatalf("recorded %+v, want the turn's one end, stopped by Jane", ends)
	}
}

// A TURN LOST WITH ITS RUN ENDS AS LOST ON THE EXECUTOR LOG, with the loss the
// run's ending recorded, and whether its reply reached anybody as the run's own
// calls say — here a turn nobody was waiting on, which has nobody to miss it.
//
// Mutation: answer only a colleague's ask on a loss, as the step did before,
// and a lost turn no colleague asked for ends nowhere.
func TestALostTurnEndsAsLostOnTheExecutorLog(t *testing.T) {
	t.Parallel()
	e, spy := loggingEngine(t)
	rigRuns(t, e, halfwayPlay)
	now := time.Now().UTC()
	e.answerLostTurn(t.Context(), sandbox.PendingRun{
		AgentHandle: "swe", TurnID: "run-9", ConversationKey: "task:9", WorkKey: "wk-9", WorkSince: now,
		Reply: string(turn.ReplyNone), TaskDescription: "fix the flake",
		Ending: &sandbox.RecordedEnding{ID: "end-1", At: now, Reason: "box_gone", Detail: "its box was lost"},
	})
	ends := spy.endsOf("run-9")
	if len(ends) != 1 {
		t.Fatalf("recorded %d ends of the lost turn, want 1", len(ends))
	}
	end := ends[0]
	if end.Ending.Outcome != executordomain.OutcomeLost || !strings.Contains(end.Ending.Failure, "box_gone") ||
		!end.Ending.Delivered || end.ConversationKey != "task:9" || end.Ask != "fix the flake" {
		t.Fatalf("the end is %+v, want the turn lost to its box, its conversation and its ask", end)
	}
}

// THE END CARRIES THE TURN'S OWN DECISION AND BREACH, NEVER THE REVIEWER'S LAST
// WORD: a turn the loop failed on a guard after its reviewer said `done` ended
// failed, saying which guard; what the reviewer said landed and what stopped a
// blocked round are carried; and a stop says who stopped it and why.
//
// Mutation: read the decision off LastReview, drop the breach's account, or
// leave the stop's author off, and a field below says something else.
func TestTheEndCarriesTheTurnsOwnDecisionAndBreach(t *testing.T) {
	t.Parallel()
	res := turn.Result{
		Decision: phase.Failed, Delivered: true,
		Breach:     &turn.Breach{Kind: types.GuardDepthCap, Detail: "three hops"},
		LastReview: &turn.Review{Decision: phase.Done, CompletedWork: "MR !42 is up"},
		LastWork:   &turn.Work{Outcome: turn.OutcomeBlocked, Evidence: "waiting on the founder"},
	}
	reply := turn.Reply{Kind: turn.ReplyTool, Surface: "chatlet"}
	ending := endingOf(res, reply, colleagueAnswer{})
	if ending.Outcome != executordomain.OutcomeFailed || ending.Decision != string(phase.Failed) ||
		!strings.Contains(ending.Failure, string(types.GuardDepthCap)) || ending.CompletedWork != "MR !42 is up" ||
		ending.BlockedOn != "waiting on the founder" || !ending.Delivered || ending.DeliveredTo != "chatlet" {
		t.Fatalf("the ending is %+v", ending)
	}
	stop := &stopError{pause: coord.SeatPause{By: "jane", Reason: "wrong repository", StopRunning: true}}
	stopped := stoppedEnding(turn.Result{Decision: phase.Done}, reply, stop, colleagueAnswer{})
	if stopped.Outcome != executordomain.OutcomeStopped || stopped.StoppedBy != "jane" ||
		stopped.StopReason != "wrong repository" || stopped.Failure != "" {
		t.Fatalf("the stopped ending is %+v", stopped)
	}
}

// A TURN THAT OWED A COLLEAGUE THE ENGINE'S ANSWER ENDS SAYING WHAT THE ENGINE
// SENT AND WHETHER IT WENT — which the turn's own calls cannot say — and a turn
// that owed none says nothing of it.
//
// Mutation: take Delivered off the turn's record for an engine's answer, which
// reads `true` by construction, or drop the kind, and a reply that never went
// reads as delivered.
func TestTheEndSaysWhatTheEngineAnsweredAColleague(t *testing.T) {
	t.Parallel()
	res := turn.Result{Decision: phase.Done, Artifact: "the answer", Delivered: true}
	reply := turn.Reply{Kind: turn.ReplyEngine}
	for _, tc := range []struct {
		name      string
		answered  colleagueAnswer
		answer    string
		delivered bool
	}{
		{"replied", colleagueAnswer{owed: true, kind: "artifact", replied: true}, "artifact", true},
		{"refused", colleagueAnswer{owed: true, kind: "artifact"}, "artifact", false},
		{"closed", colleagueAnswer{owed: true, kind: "artifact", closed: true}, "channel_closed", false},
	} {
		ending := endingOf(res, reply, tc.answered)
		if ending.Answer != tc.answer || ending.Delivered != tc.delivered || ending.DeliveredTo != replyA2A {
			t.Errorf("%s: the ending is %+v", tc.name, ending)
		}
	}
	if ending := endingOf(res, turn.Reply{Kind: turn.ReplyNone}, colleagueAnswer{}); ending.Answer != "" {
		t.Errorf("a turn that owed no answer ended saying %q", ending.Answer)
	}
	for _, tc := range []struct {
		res  turn.Result
		err  error
		kind string
	}{
		{res, nil, "artifact"},
		{turn.Result{Decision: phase.Failed, Breach: &turn.Breach{Kind: types.GuardDepthCap}}, nil, "guard"},
		{turn.Result{Decision: phase.Skipped}, nil, "unproduced"},
		{turn.Result{}, &runLostError{reason: "box_gone"}, "lost"},
	} {
		if _, kind := answerContent(tc.res, tc.err); kind != tc.kind {
			t.Errorf("answerContent(%+v, %v) is a %q, want %q", tc.res.Decision, tc.err, kind, tc.kind)
		}
	}
}
