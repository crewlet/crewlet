package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// THE LEARNING WRITE SIDE, end to end.
//
// The claim being tested is not that any one worker works — the learning
// suite covers each against fakes. It is that a real node, having taken a
// real turn, ends up with rows in its store: the completed-turn event is
// published with the fields the gates read, a queue consumer picks it up,
// the dispatcher resolves the seat against the live epoch, and three
// writers land three different kinds of row.
//
// Every one of those was a wire that was not connected. The read side was,
// and it looked healthy: it queried, found nothing, and rendered nothing —
// which is indistinguishable from a company that is simply young.

// wakeWithMessage publishes a substantive trigger from an identifiable
// person, which is what the profiler needs and the recon gate does not skip.
func wakeWithMessage(t *testing.T, n *node, handle string) {
	t.Helper()
	body := "can you send the weekly numbers, plainly, no preamble"
	ev := events.New(types.ExternalNotification{
		NotificationSource: "mattermost", SourceEventType: "posted",
		Sender: "sam", Subject: "weekly numbers",
		Body: body, SalientBody: &body,
		Metadata: map[string]string{
			notify.ActorField:       "u-sam",
			notify.ChannelKindField: string(types.ChannelDM),
		},
	}, events.TraceContext{})
	if err := n.engine.Backends().Queue.Publish(t.Context(),
		topics.AgentInbox(handle), ev); err != nil {
		t.Fatalf("wake %s: %v", handle, err)
	}
}

// awaitRows polls until the rows exist.
//
// Through the suite's own [waitFor] rather than a second deadline of its
// own: this is the longest path any of these tests drive — a turn completes,
// the event is published, a queue consumer picks it up, the dispatcher runs
// three workers and each writes — so a tighter budget here is the one that
// fails first when the machine is loaded, and a suite with two disagreeing
// timeouts fails in whichever place happens to hold the smaller one.
//
// A read error is FATAL rather than retried: it means the store is
// unreadable, which no amount of waiting fixes, and swallowing it would
// spend the whole budget to report a timeout instead of the real fault.
func awaitRows(t *testing.T, what string, found func() (int, error)) {
	t.Helper()
	waitFor(t, what, func() bool {
		n, err := found()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return n > 0
	})
}

func TestACompletedTurnLeavesAnEpisodeBehind(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")
	// A turn that calls nothing has engaged with nothing, and every
	// learning worker correctly skips it — see scriptedModel.engages.
	n.model.engageOnExecute()
	wakeWithMessage(t, n, "ceo")
	waitForTurn(t, n)

	episodes := learning.NewEpisodes(n.engine.Backends().Store)
	awaitRows(t, "the turn's episode", func() (int, error) {
		rows, err := episodes.Recent(context.Background(), "ceo", 10)
		return len(rows), err
	})

	rows, err := episodes.Recent(context.Background(), "ceo", 10)
	if err != nil {
		t.Fatalf("Recent: %v", err)
	}
	ep := rows[0]
	if ep.Role != "CEO" {
		t.Errorf("role = %q, want the seat's", ep.Role)
	}
	if ep.ReviewOutcome != "done" {
		t.Errorf("review outcome = %q, want the turn's decision", ep.ReviewOutcome)
	}
	// THE WORK KEY IS WHAT DEDUPES, and it only reaches the row if the
	// turn event carried it: an episode keyed on nothing lands twice the
	// first time two nodes complete one trigger.
	if ep.WorkKey == "" {
		t.Error("the episode carries no work key, so nothing dedupes it")
	}
	if ep.TaskSummary == "" {
		t.Error("the episode carries no task summary, so recall can never match it")
	}
}

func TestACompletedTurnLeavesADiaryRowBehind(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")
	// A turn that calls nothing has engaged with nothing, and every
	// learning worker correctly skips it — see scriptedModel.engages.
	n.model.engageOnExecute()
	wakeWithMessage(t, n, "ceo")
	waitForTurn(t, n)

	seat, ok := n.engine.Registry().ByHandle("ceo")
	if !ok {
		t.Fatal("no CEO seat")
	}
	diary := learning.NewDiary(n.engine.Backends().Store)
	awaitRows(t, "the classifier's diary row", func() (int, error) {
		rows, err := diary.Recent(context.Background(), seat.AgentID.String(),
			time.Now().UTC(), 10)
		return len(rows), err
	})
}

func TestACompletedTurnLeavesACounterpartyProfileBehind(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")
	// A turn that calls nothing has engaged with nothing, and every
	// learning worker correctly skips it — see scriptedModel.engages.
	n.model.engageOnExecute()
	wakeWithMessage(t, n, "ceo")
	waitForTurn(t, n)

	counterparties := learning.NewCounterparties(n.engine.Backends().Store)
	subject := learning.Subject{
		ExternalID: "u-sam", Platform: "mattermost", Name: "sam",
	}
	awaitRows(t, "the sender's profile", func() (int, error) {
		_, found, err := counterparties.Get(context.Background(), "ceo", subject)
		if !found {
			return 0, err
		}
		return 1, err
	})

	got, _, err := counterparties.Get(context.Background(), "ceo", subject)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.InteractionCount != 1 {
		t.Errorf("interaction count = %d, want the one message", got.InteractionCount)
	}
	if got.Traits["reply_style"] != "plain numbers, no preamble" {
		t.Errorf("traits = %v, want the patch the model answered with", got.Traits)
	}
}

// THE GATES READ FIELDS THE ENGINE HAS TO PUBLISH. Every one of these was
// absent from the completed-turn event, and their absence failed OPEN-
// LOOKING: an empty tool sequence reads as "the agent engaged with nothing",
// which skips every worker on exactly the successful turns worth learning
// from — silently, with the dispatcher reporting a clean pass.
func TestTheCompletedTurnEventCarriesWhatReflectionGatesOn(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")
	n.model.engageOnExecute()

	completed := make(chan types.TurnCompleted, 4)
	if err := n.engine.Backends().Queue.Subscribe(t.Context(),
		topics.Event(types.TurnCompleted{}.EventType()), "e2e-turn-watch",
		queueHandler(completed)); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	wakeWithMessage(t, n, "ceo")
	select {
	case tc := <-completed:
		if len(tc.ToolSequence) == 0 && tc.ReviewOutcome == "done" {
			t.Error("a done turn published no tool sequence, which every " +
				"engagement gate reads as 'the agent did nothing'")
		}
		if tc.PlanDecision == "" {
			t.Error("no plan decision, so the skip gate cannot fire")
		}
		if len(tc.Interactions) == 0 {
			t.Error("no interactions, so no counterparty can ever be profiled")
		}
		if tc.Interactions[0].Sender.ExternalID != "u-sam" {
			t.Errorf("sender = %+v, want the resolved actor",
				tc.Interactions[0].Sender)
		}
		if tc.Interactions[0].ChannelKind != types.ChannelDM {
			t.Errorf("channel kind = %q, want the canonical dm the parser stamped",
				tc.Interactions[0].ChannelKind)
		}
		if tc.TurnID == "" {
			t.Error("no turn id, so nothing the workers write can be deduped")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no turn was ever completed")
	}
}

// queueHandler forwards completed turns to a test channel.
//
// NON-BLOCKING on a full buffer, and it always acks: a handler that blocked
// would hold the consumer's goroutine for the rest of the run, and a nak
// would redeliver a turn the test has already read.
func queueHandler(out chan<- types.TurnCompleted) queue.Handler {
	return func(_ context.Context, ev *events.Event) queue.Result {
		if tc, ok := events.DataAs[*types.TurnCompleted](ev); ok {
			select {
			case out <- *tc:
			default:
			}
		}
		return queue.Ack()
	}
}
