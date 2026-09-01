package engine

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
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

func resumed(conversation string) resumeInput {
	return resumeInput{
		Run: sandbox.PendingRun{
			TurnID:          "wk-1",
			AgentHandle:     "swe",
			ConversationKey: conversation,
		},
		Turn: &turnctx.Turn{ID: "wk-1", Seat: &org.Role{Name: "Engineer", DeclaredHandle: "swe"}},
	}
}

// The finding this commit closes: a turn that ended on the resume path wrote
// no conversation entry at all, so the thread's history stopped at the moment
// the coding run detached and the seat's next turn planned the work again.
func TestAResumedTurnRecordsWhatItSaidToTheConversation(t *testing.T) {
	t.Parallel()
	e, conversations := resumingEngine(t)
	ctx := context.Background()

	e.recordResume(ctx, resumed("slack:C1"), turn.Result{
		Decision:   phase.Done,
		Artifact:   "shipped the branch and opened the MR",
		LastReview: &turn.Review{CompletedWork: "MR !42 is up"},
	})

	got, err := conversations.History(ctx, "swe", "slack:C1", 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("history = %d entries, want the resumed turn's own", len(got))
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

	e.recordResume(ctx, resumed("slack:C1"), turn.Result{
		Suspended: true, Decision: phase.SelfIterate, Artifact: "half a thought",
	})

	got, err := conversations.History(ctx, "swe", "slack:C1", 0)
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

	d.RecordSession(ctx, "swe", "slack:C1", "wk-1", turn.Result{
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

	e.recordResume(ctx, resumed(""), turn.Result{Decision: phase.Done, Artifact: "x"})

	got, err := conversations.History(ctx, "swe", "", 0)
	if err != nil {
		t.Fatalf("History: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("history = %d entries, want none", len(got))
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
	(&Engine{dispatch: &Dispatcher{}}).recordResume(ctx, resumed("slack:C1"), done)
	// No dispatcher at all — a test or a partially built engine.
	(&Engine{}).recordResume(ctx, resumed("slack:C1"), done)
	// A ledger that refuses the write.
	(&Engine{dispatch: &Dispatcher{Conversations: failingResumeConversations{}}}).
		recordResume(ctx, resumed("slack:C1"), done)
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

// The conversation key has to REACH the row for any of the above to fire: the
// launch is the only place that knows it, and the resume — another process,
// days later — cannot recover it from a trigger that may be long gone.
func TestTheTurnCarriesItsConversationToWorkItDetaches(t *testing.T) {
	t.Parallel()
	tel := turnTelemetry{handle: "swe", convKey: "slack:C1"}
	company := &Company{Org: &org.Organization{
		Name:  "Acme",
		Roles: []*org.Role{{Name: "Engineer", DeclaredHandle: "swe"}},
	}}
	got := tel.runnerTurn(company, "wk-1", 0, nil)
	if got.Context == nil {
		t.Fatal("the runner turn carries no turn context")
	}
	if got.Context.ConversationKey != "slack:C1" {
		t.Errorf("turn context conversation = %q, want the trigger's", got.Context.ConversationKey)
	}
}
