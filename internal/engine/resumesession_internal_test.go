package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/tools"
)

// resumingEngine is an engine with nothing but the ledger a resumed turn
// writes to, and a fixed clock.
func resumingEngine(t *testing.T) (*Engine, ledgerstore.Conversations) {
	t.Helper()
	conversations := ledgerstore.NewMemoryConversations()
	at := time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC)
	return &Engine{dispatch: &Dispatcher{
		Conversations: conversations,
		Now:           func() time.Time { return at },
	}}, conversations
}

// resumed is one parked run coming back, with BOTH of its conversation values
// stated at every call site.
//
// THE TWO ARE NEVER EQUAL HERE, and that is the whole point of the pair: they
// differ for exactly one surface, a direct message, where the partition is the
// thread the kick-off trigger arrived in and the conversation is the whole DM
// line. A fixture that set only one left the accessor falling back to the
// other at every call site in this file, so the ledger key the split exists to
// fix was filed under whichever field happened to be there — and reverting
// [Engine.recordResume] to the partition passed every case below.
func resumed(conversation, partition string) resumeInput {
	return resumeInput{
		Run: sandbox.PendingRun{
			TurnID:          "wk-1",
			AgentHandle:     "swe",
			ConversationKey: conversation,
			PartitionKey:    partition,
		},
		Turn: &turnctx.Turn{RunID: "run-1", WorkKey: "wk-1", Seat: &org.Role{Name: "Engineer", DeclaredHandle: "swe"}},
	}
}

// theDM is the pair a run launched from a direct message's thread carries: the
// conversation a resume reports back to, and the batch it was launched from.
const (
	theDM       = "slack:D1"
	theDMThread = "slack:D1:root-1"
)

// The finding this commit closes: a turn that ended on the resume path wrote
// no conversation entry at all, so the thread's history stopped at the moment
// the coding run detached and the seat's next turn planned the work again.
func TestAResumedTurnRecordsWhatItSaidToTheConversation(t *testing.T) {
	t.Parallel()
	e, conversations := resumingEngine(t)
	ctx := context.Background()

	e.recordResume(ctx, resumed(theDM, theDMThread), turn.Result{
		Decision:   phase.Done,
		Delivered:  true,
		Artifact:   "shipped the branch and opened the MR",
		LastReview: &turn.Review{CompletedWork: "MR !42 is up"},
	})

	got, err := conversations.History(ctx, "swe", theDM, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("history = %d entries, want the resumed turn's own", len(got))
	}
	// UNDER THE CONVERSATION AND NOT THE BATCH, which is the half a fixture
	// stating one value could not assert: the seat's next turn on this DM
	// reads the conversation back, and a coding turn filed under the thread
	// it happened to be triggered in is history that turn never looks up.
	batch, err := conversations.History(ctx, "swe", theDMThread, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(batch) != 0 {
		t.Errorf("the resumed turn was filed under the inbox batch as well (%d entries): "+
			"the next turn on this conversation reads it back from neither", len(batch))
	}
	if got[0].Reply != "shipped the branch and opened the MR" {
		t.Errorf("reply = %q", got[0].Reply)
	}
	if got[0].Decision != "done" {
		t.Errorf("decision = %q", got[0].Decision)
	}
	// The reviewer's gloss survives, exactly as it does on the inbox path:
	// a `done` round appends no iteration record of its own, so this is the
	// only place it is written down.
	if got[0].CompletedWork != "MR !42 is up" {
		t.Errorf("completed work = %q", got[0].CompletedWork)
	}
}

// A resume that suspends AGAIN has not finished. Recording it would file a
// half-turn's artifact as the seat's answer, and the completion that
// eventually lands would file a second entry for the same turn.
func TestAResumeThatSuspendsAgainRecordsNothing(t *testing.T) {
	t.Parallel()
	e, conversations := resumingEngine(t)
	ctx := context.Background()

	e.recordResume(ctx, resumed(theDM, theDMThread), turn.Result{
		Suspended: true, Decision: phase.SelfIterate, Artifact: "half a thought",
	})

	got, err := conversations.History(ctx, "swe", theDM, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("history = %d entries, want none until the turn finishes: %+v", len(got), got)
	}
}

// The same rule on the DISPATCH path, where it was missing. A turn that
// suspends is acked and recorded as worked — correctly, the trigger is not
// coming back — but it has said nothing, so writing its self_iterate and its
// empty artifact to the thread files a decision the seat never made as its
// answer, and the next turn on that thread reads it back as what happened.
func TestASuspendedTurnFilesNothingAgainstItsConversation(t *testing.T) {
	t.Parallel()
	conversations := ledgerstore.NewMemoryConversations()
	d := &Dispatcher{Conversations: conversations}
	ctx := context.Background()

	d.RecordSession(ctx, "swe", "slack:C1", "run-1", "wk-1", "@ana: fix CI", turn.Result{
		Suspended: true, Decision: phase.SelfIterate,
	}, time.Now().UTC())

	got, err := conversations.History(ctx, "swe", "slack:C1", 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("a suspended turn filed %d entries: %+v", len(got), got)
	}
}

// A run with no conversation — a scheduled fire, an internal trigger — writes
// nothing rather than collecting every such turn under one empty key and
// feeding it back to the next one as history.
func TestAResumedTurnWithNoConversationRecordsNothing(t *testing.T) {
	t.Parallel()
	e, conversations := resumingEngine(t)
	ctx := context.Background()

	e.recordResume(ctx, resumed("", ""), turn.Result{Decision: phase.Done, Artifact: "x"})

	got, err := conversations.History(ctx, "swe", "", 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("history = %d entries, want none", len(got))
	}
}

// A ROW FROM BEFORE THE SPLIT CARRIES ONLY THE PARTITION, and the resume files
// under it rather than recording nothing.
//
// The fallback is pinned on both sides of the seam on purpose: [PendingRun] has
// it because nothing rewrites a parked run and one waits for a person, and
// here because this is the frame that would otherwise write an empty key — a
// resumed turn that recorded nothing at all is the gap [Engine.recordResume]
// exists to close, and such a row would fall straight back into it.
func TestAResumedRunFromBeforeTheSplitFilesUnderItsPartition(t *testing.T) {
	t.Parallel()
	e, conversations := resumingEngine(t)
	ctx := context.Background()

	e.recordResume(ctx, resumed("", theDMThread), turn.Result{
		Decision: phase.Done, Delivered: true, Artifact: "shipped the branch",
	})

	got, err := conversations.History(ctx, "swe", theDMThread, 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("history = %d entries under the one key the row carries, want the "+
			"resumed turn's own: a pre-split row records nothing at all", len(got))
	}
}

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

	tel := (&Engine{}).describeResume(context.Background(), company,
		resumed(theDM, theDMThread))

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

// Bookkeeping fails open on both paths: the turn has already delivered, and
// an engine with no conversation ledger at all is the ordinary single-node
// case rather than a fault.
func TestRecordingAResumeNeverFailsTheRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	done := turn.Result{Decision: phase.Done, Artifact: "x"}

	// No ledger wired.
	(&Engine{dispatch: &Dispatcher{}}).recordResume(ctx, resumed(theDM, theDMThread), done)
	// No dispatcher at all — a test or a partially built engine.
	(&Engine{}).recordResume(ctx, resumed(theDM, theDMThread), done)
	// A ledger that refuses the write.
	(&Engine{dispatch: &Dispatcher{Conversations: failingResumeConversations{}}}).
		recordResume(ctx, resumed(theDM, theDMThread), done)
}

type failingResumeConversations struct{}

func (failingResumeConversations) Append(context.Context, string, string, ledger.Session, string, time.Time, int) error {
	return context.DeadlineExceeded
}

func (failingResumeConversations) History(context.Context, string, string, int) ([]ledger.Session, error) {
	return nil, nil
}

func (failingResumeConversations) Threads(context.Context, string, int) ([]ledgerstore.Thread, error) {
	return nil, nil
}

func (failingResumeConversations) Purge(context.Context, time.Time) (int64, error) { return 0, nil }

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
	got := tel.runnerTurn(company, 0, nil, "fix the failing test", turn.ToolReply(""))
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
// epoch an apply swapped in after the admission. The admitted company carries
// no turn settings, so the turn stops at its first round: reaching that round
// at all is the proof, since it needs a runner built for the seat, and a runner
// built from the engine's current company is refused before it exists.
func TestAResumeRunsInTheEpochThatAdmittedIt(t *testing.T) {
	t.Parallel()
	admitted, seat := modeCompany(t, "claude-code", false, "")
	admitted.Tools = tools.NewRegistry()
	e, _ := resumingEngine(t)
	e.backends = &Backends{Queue: memory.New()}
	e.epoch.current.Store(&Company{
		Config: admitted.Config, Models: admitted.Models, Tools: admitted.Tools,
		Org: &org.Organization{Name: admitted.Org.Name},
	})
	in := resumed(theDM, theDMThread)
	in.Company = admitted
	in.Run.AgentHandle = seat.Handle()
	in.Turn = &turnctx.Turn{
		RunID: in.Run.TurnID, WorkKey: in.Run.WorkKey,
		Seat: seat, Org: admitted.Org,
	}

	err := e.resumeTurn(t.Context(), in)
	if err == nil || !strings.Contains(err.Error(), "resume round 1") {
		t.Fatalf("resumeTurn() = %v, want the turn to reach its first round: a runner "+
			"built from the engine's current company is refused before it", err)
	}
}

// A PARKED ROW OUTLIVES THE BUILD THAT WROTE IT, so who is waiting for a
// resumed turn is read off it defensively.
//
// An ABSENT value is [turn.NoReply], the reading [sandbox.PendingRun] states
// for it: `reply` is an omitempty column, nothing rewrites a parked row, and a
// run launched before the field existed simply has none. Refused instead, those
// runs could never be resumed, so a box that had already done the work was
// collected and its answer dropped.
//
// A value this build does not recognise is a ROUTING refusal rather than a
// default, so the completion reaches a peer that can read it instead of being
// settled here against a guess at who is waiting.
func TestAParkedRunsReplyIsReadOffItsRowDefensively(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		stored string
		want   turn.Reply
	}{
		{"a row written before the field existed", "", turn.NoReply()},
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
	_, err := resumeReply(sandbox.PendingRun{TurnID: "wk-1", Reply: "whisper"})
	if !errors.Is(err, sandbox.ErrResumeUnavailable) {
		t.Fatalf("a reply kind this build does not know = %v, want a routing refusal so "+
			"the completion reaches a node that can read it", err)
	}
}
