package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// resumingEngine is an engine with nothing but a dispatcher on a fixed clock.
func resumingEngine(t *testing.T) *Engine {
	t.Helper()
	at := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	return &Engine{dispatch: &Dispatcher{Now: func() time.Time { return at }}}
}

// resumed is one parked run coming back, with BOTH of its conversation values
// stated at every call site.
//
// THE TWO ARE NEVER EQUAL HERE, and that is the whole point of the pair: they
// differ for exactly one surface, a direct message, where the partition is the
// thread the kick-off trigger arrived in and the conversation is the whole DM
// line — so reverting [Engine.describeResume] to the partition fails a case
// below.
func resumed(conversation, partition string) resumeInput {
	return resumeInput{
		Run: sandbox.PendingRun{
			TurnID:          "wk-1",
			AgentHandle:     "swe",
			ConversationKey: conversation,
			PartitionKey:    partition,
			// Every launch writes who is waiting; a resume refuses a row
			// that says nothing.
			Reply: "tool",
		},
		// BEGUN A MINUTE AGO: the start is what the resumed half mints at
		// while it is inside the ledger's horizon, and a zero one would be
		// rebased onto the resume — a coordination store's business, not
		// this helper's.
		Turn: &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1",
			WorkSince: time.Now().UTC().Add(-time.Minute),
			Seat:      &org.Role{Name: "Engineer", DeclaredHandle: "swe"}},
	}
}

// theDM is the pair a run launched from a direct message's thread carries: the
// conversation a resume reports back to, and the batch it was launched from.
const (
	theDM       = "slack:D1"
	theDMThread = "slack:D1:root-1"
)

// THE TWO VALUES REACH THE ROW IN THE RIGHT FIELDS, which nothing asserted.
//
// This is the launch's own mapping, from the running turn onto the record a
// resume days later reads back: the conversation the answer is matched on and
// the turn reports through, and the batch the kick-off trigger arrived in. The
// two fields sat under names that meant each other's concept, so the
// assignment read as its own opposite and a crossed pair compiled — both are
// strings — and cost a run every answer it was ever given. What a swap looks
// like from here is a row whose conversation is a thread nobody replies on.
func TestADetachedRunsRowKeepsThePartitionAndTheConversationApart(t *testing.T) {
	t.Parallel()
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "swe"}
	l := &agentLauncher{
		seat: seat,
		turn: &turnctx.Turn{
			RunID: "run-1", WorkKey: "wk-1", Seat: seat,
			ConversationKey: theDM, PartitionKey: theDMThread,
			Reply: "tool",
		},
	}

	ref := l.runTurnRef(context.Background())
	if ref.ConversationKey != theDM {
		t.Errorf("the row's conversation = %q, want the DM line the person answers on", ref.ConversationKey)
	}
	if ref.PartitionKey != theDMThread {
		t.Errorf("the row's partition = %q, want the batch the run was launched from", ref.PartitionKey)
	}
	// AND THE OTHER CROSSED PAIR, which is the same hazard one identity
	// down: the run is this execution and the work key is what a redelivery
	// reproduces (ADR-0017), both are strings, and a row that swapped them
	// would resume under an id no completion poll is watching.
	if ref.TurnID != "run-1" {
		t.Errorf("the row's run = %q, want this execution's own id", ref.TurnID)
	}
	if ref.WorkKey != "wk-1" {
		t.Errorf("the row's work key = %q, want the unit a redelivery reproduces", ref.WorkKey)
	}
	if ref.Reply != "tool" || ref.AgentHandle != "swe" {
		t.Errorf("the rest of the row is wrong too: %+v", ref)
	}
}

// AND A RESUME READS THEM BACK OUT OF THE ROW INTO THE RIGHT FIELDS.
//
// The launch's mapping had a test; the resume's did not, and it is the same
// crossed pair in the other direction — the row's ConversationKey is the
// IDENTITY, so reading it as the partition collapses the two into one value
// the moment a run parks a second time. A re-parked row then carries the bare
// DM channel where its first launch carried the thread, and
// sandbox.ConversationRef.Best loses the one fact that tells two questions on
// one direct-message line apart.
func TestAResumeKeepsThePartitionAndTheConversationApart(t *testing.T) {
	t.Parallel()
	company := &Company{Org: &org.Organization{
		Name:  "Acme",
		Roles: []*org.Role{{Name: "Engineer", DeclaredHandle: "swe"}},
	}}

	tel := mustDescribeResume(t, &Engine{}, company, resumed(theDM, theDMThread))

	if tel.convKey != theDM {
		t.Errorf("the resumed turn's conversation = %q, want the DM line the "+
			"person answers on", tel.convKey)
	}
	if tel.partKey != theDMThread {
		t.Errorf("the resumed turn's partition = %q, want the batch the run "+
			"was launched from: a run that suspends again writes this back "+
			"onto its row", tel.partKey)
	}
}

// Owing a resumed turn's end fails open: the turn has already delivered, and
// an engine with no sandbox runtime — a test, a partially built engine — has no
// run record to owe it on, which is no fault of the run's.
//
// Mutation: drop [Engine.oweExecutorEnd]'s return on a nil runtime and this
// panics.
func TestOwingAResumesEndNeverFailsTheRun(t *testing.T) {
	t.Parallel()
	done := turn.Result{Decision: phase.Done, Artifact: "x"}
	(&Engine{}).recordResume(context.Background(), resumed(theDM, theDMThread), done,
		turn.Reply{}, colleagueAnswer{})
}

// THREE FACTS HAVE TO REACH THE ROW for any of the above to fire, and the
// launch is the only place that knows them: the conversation to report back
// to, the brief the turn was working on, and who is waiting for it. The
// resume — another process, days later — cannot recover any of them from a
// trigger that may be long gone.
func TestTheTurnCarriesWhatWorkItDetachesWillNeed(t *testing.T) {
	t.Parallel()
	// BOTH CONVERSATION VALUES, and different ones: this is where the pair
	// starts, and a fixture that made them equal would let either field be
	// copied into both and still pass.
	tel := turnTelemetry{
		handle: "swe", convKey: theDM, partKey: theDMThread,
		runID: "run-1", workKey: "wk-1",
	}
	company := &Company{Org: &org.Organization{
		Name:  "Acme",
		Roles: []*org.Role{{Name: "Engineer", DeclaredHandle: "swe"}},
	}}
	got := tel.runnerTurn(company, 0, nil, "fix the failing test", turn.ToolReply(""), nil)
	if got.Context == nil {
		t.Fatal("the runner turn carries no turn context")
	}
	if got.Context.ConversationKey != theDM {
		t.Errorf("turn context conversation = %q, want the trigger's", got.Context.ConversationKey)
	}
	// AND THE PARTITION BESIDE IT, in its own field. A detached run's row
	// states both, and this frame is the only one that holds either — so a
	// turn that carried one value twice would write a row whose answer match
	// and whose report-back key are the same wrong string.
	if got.Context.PartitionKey != theDMThread {
		t.Errorf("turn context partition = %q, want the batch the trigger arrived in",
			got.Context.PartitionKey)
	}
	if got.Context.Task != "fix the failing test" {
		t.Errorf("turn context task = %q", got.Context.Task)
	}
	if got.Context.Reply != turn.ToolReply("").String() {
		t.Errorf("turn context reply = %q, want the delivery obligation", got.Context.Reply)
	}
}

// A RESUME RUNS IN THE EPOCH THAT ADMITTED IT. The resumer finds the seat in
// the company it read, and the turn it hands on must run in that same company:
// a second read of the engine's company is the next epoch once an apply lands
// between the two, the seat can be gone from it, and the run then failed with
// "not an agent seat" rather than being routed to a node that has the seat.
//
// The engine's current company here has no seats at all, standing in for the
// epoch an apply swapped in after the admission. A resume hands the run's
// result to the seat's reviewer, whose model is a harness that is not installed,
// so the turn stops at the reviewer's first round: reaching that round at all
// is the proof, since it needs a runner built for the seat, and a runner built
// from the engine's current company is refused before it exists.
func TestAResumeRunsInTheEpochThatAdmittedIt(t *testing.T) {
	t.Parallel()
	admitted, seat := executorCompany(t)
	admitted.Tools = tools.NewRegistry()
	e := resumingEngine(t)
	e.backends = &Backends{Queue: memory.New()}
	// A SESSION HOST, as every engine `crewlet run` builds has one: the
	// runner refuses to be built without the host its phases serve their
	// tools on, and that refusal would stop the resume before the round
	// this case is about.
	e = withSessions(t, e)
	e.epoch.current.Store(&Company{
		Config: admitted.Config, Models: admitted.Models, Tools: admitted.Tools,
		Org: &org.Organization{Name: admitted.Org.Name},
	})
	in := resumed(theDM, theDMThread)
	in.Company = admitted
	in.Run.AgentHandle = seat.Handle()
	// A RUN MINTED NOW, with no unit of work: its ids are seeded from the
	// run and minted at its start, which is inside the ledger's horizon, so
	// the resume needs no coordination store to record a rebase in.
	in.Run.TurnID = newRunID()
	in.Turn = resumedTurn(in.Run, seat, admitted.Org, turnctx.NewHolds(in.Run.CodeHolds))

	err := e.resumeTurn(t.Context(), in)
	if err == nil || !strings.Contains(err.Error(), "review round 1") {
		t.Fatalf("resumeTurn() = %v, want the turn to reach its first round: a runner "+
			"built from the engine's current company is refused before it", err)
	}
}

// A RESUMED SEGMENT'S LAUNCHER KNOWS THE WINDOW ITS RUN'S LAST RELAUNCH WAS
// REFUSED IN. The release that handed back the claim whose relaunch the seat's
// budget refused below its floor counted that window onto the job's record, and
// the segment the resume rebuilds launches the turn's next relaunch from it:
// its launcher reads it there, so that relaunch, in a later window, waits for
// its floor no longer ([TestALaunchRefusedBelowItsFloorWaitsOneTurnoverAtMost]).
// A record naming none hands the launcher the zero window, never nothing,
// since a resumed launcher takes no park's record a dispatch left on the seat.
//
// Mutation: build the resumed segment's launcher without the job's record's
// window, or with none at all, and a case goes red.
func TestAResumedSegmentsLauncherKnowsTheWindowItsRelaunchWasRefusedIn(t *testing.T) {
	t.Parallel()
	for name, window := range map[string]sandbox.BudgetWindow{
		"a record naming the window": {Period: "day", Label: "2026-09-23"},
		"a record naming none":       {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			admitted, seat := executorCompany(t)
			admitted.Tools = tools.NewRegistry()
			e := resumingEngine(t)
			e.backends = &Backends{Queue: memory.New()}
			e = withSessions(t, e)
			e.epoch.current.Store(admitted)
			in := resumed(theDM, theDMThread)
			in.Company = admitted
			in.Run.AgentHandle = seat.Handle()
			in.Run.TurnID = newRunID()
			in.Run.Launch.RelaunchRefusedIn = window
			in.Turn = resumedTurn(in.Run, seat, admitted.Org, turnctx.NewHolds(in.Run.CodeHolds))
			input, _, err := e.parkedTurnRunner(t.Context(), admitted, in, nil)
			if err != nil {
				t.Fatalf("parkedTurnRunner: %v", err)
			}
			l, ok := input.Launcher.(*agentLauncher)
			if !ok {
				t.Fatalf("the resumed segment's launcher is %T, want the engine's", input.Launcher)
			}
			if l.relaunchRefusedIn == nil || *l.relaunchRefusedIn != window {
				t.Fatalf("the resumed segment's launcher reads %v, want the job's record's window %+v",
					l.relaunchRefusedIn, window)
			}
		})
	}
}

// A PARKED ROW OUTLIVES THE BUILD THAT WROTE IT, so who is waiting for a
// resumed turn is read off it defensively.
//
// A value this build does not recognise — a successor's kind, or none at all —
// is a ROUTING refusal rather than a default, so the completion reaches a peer
// that can read it instead of being settled here against a guess at who is
// waiting.
func TestAParkedRunsReplyIsReadOffItsRowDefensively(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		stored string
		want   turn.Reply
	}{
		{"nobody is waiting", "none", turn.NoReply()},
		{"somebody is waiting, on no surface the trigger named", "tool", turn.ToolReply("")},
		{"somebody is waiting on a named surface", "tool:mattermost", turn.ToolReply("mattermost")},
		{"a colleague asked over A2A", "engine", turn.EngineReply()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := resumeReply(sandbox.PendingRun{TurnID: "wk-1", Reply: tc.stored})
			if err != nil {
				t.Fatalf("resumeReply(%q) = %v", tc.stored, err)
			}
			if got != tc.want {
				t.Errorf("resumeReply(%q) = %+v, want %+v", tc.stored, got, tc.want)
			}
		})
	}
	for _, stored := range []string{"whisper", ""} {
		_, err := resumeReply(sandbox.PendingRun{TurnID: "wk-1", Reply: stored})
		if !errors.Is(err, sandbox.ErrResumeUnavailable) {
			t.Fatalf("reply %q = %v, want a routing refusal so the completion "+
				"reaches a node that can read it", stored, err)
		}
	}
}
