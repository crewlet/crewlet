package assistant_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// answerAboutENG7 is what the scripted model says after reading ENG-7.
const answerAboutENG7 = "ENG-7 rotates the signing keys."

// withoutApps is p holding no app at all: a person whose access was taken away.
func withoutApps(p iam.Principal) iam.Principal {
	p.Grants = nil
	return p
}

// A MESSAGE IS SHOWN ONLY TO A READER WHO STILL HOLDS WHAT ITS TURN READ: a
// reload by a person who lost the app the answer was read through shows a
// notice in its place, and the next turn's model is never handed it.
//
// Mutation: render or re-read messages without their apps and the answer
// quoting the tracker reaches a person who can no longer open it.
func TestAMessageIsHiddenFromAReaderWhoLostItsApps(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)}, Handler: task,
		Rounds: []evals.Round{readTask(), evals.Say(answerAboutENG7), evals.Say("Ask me again.")}})
	accepted, err := f.Ask("read", "What is ENG-7?", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(accepted.TurnID, 0)
	shownTo := func(p iam.Principal) []string {
		got, err := f.Svc.Transcript(t.Context(), p, f.Conversation)
		if err != nil {
			t.Fatal(err)
		}
		var texts []string
		for _, m := range got.Messages {
			if m.Role == wire.RoleAssistant && m.Text != "" {
				texts = append(texts, m.Text)
			}
		}
		return texts
	}
	if got := shownTo(f.Person); len(got) != 1 || got[0] != answerAboutENG7 {
		t.Fatalf("the reader who holds Worklet is shown %q", got)
	}
	lost := withoutApps(f.Person)
	if got := shownTo(lost); len(got) != 1 || strings.Contains(got[0], "ENG-7") {
		t.Errorf("a reader who lost Worklet is shown %q, want a notice in its place", got)
	}

	f.Person = lost
	next, err := f.Ask("next", "Remind me?", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.Follow(next.TurnID, 0)
	requests := f.Model.Requests()
	for _, m := range requests[len(requests)-1].Messages {
		if strings.Contains(m.Content, "signing keys") {
			t.Errorf("the next turn's model was handed the hidden answer: %q", m.Content)
		}
	}
}

// readThenProposeRig is a rig whose turn reads ENG-7 through Worklet and
// then proposes a task, parking on it.
func readThenProposeRig(t *testing.T) (*assistanttest.Rig, wire.TurnAccepted, assistantdomain.Proposal) {
	t.Helper()
	f := assistanttest.NewRig(t, evals.RigOptions{
		Tools: []catalog.Tool{getTask(t), assistanttest.Tool(t, "create_task")},
		Acts:  []string{"create_work_item"}, Handler: task,
		Rounds: []evals.Round{readTask(),
			evals.Call("c2", "create_task", map[string]any{"title": "Rotate keys again"}),
			evals.Say("Created it.")}})
	accepted, err := f.Ask("propose", "Read ENG-7 and file a follow-up", nil)
	if err != nil {
		t.Fatal(err)
	}
	if done := evals.Last(f.Follow(accepted.TurnID, 0), wire.EventDone); done == nil ||
		done.Done.State != wire.TurnWaitingForApproval {
		t.Fatalf("the proposing turn ended %+v", done)
	}
	return f, accepted, f.Store.Proposals()[0]
}

// A PARKED LOOP IS NEVER HANDED BACK TO THE MODEL FOR A PERSON WHO LOST WHAT
// IT READ: Carries says so before an approval sends anything, and a decision
// that reaches the turn anyway ends it failed (access withdrawn) instead of
// resuming it, its card shown at its state now.
//
// Mutation: resume without checking the parked loop's apps and the model is
// handed the tracker answer it read, for a person who can no longer open it.
func TestAParkedLoopIsNotResumedForAPersonWhoLostItsApps(t *testing.T) {
	t.Parallel()
	f, accepted, p := readThenProposeRig(t)
	lost := withoutApps(f.Person)
	carries, err := f.Svc.Carries(iam.WithPrincipal(context.Background(), f.Person),
		f.Person.ID, f.Conversation, accepted.TurnID)
	if err != nil || !carries {
		t.Fatalf("the person who holds Worklet: Carries = %v, %v", carries, err)
	}
	ctx := iam.WithPrincipal(context.Background(), lost)
	if carries, err = f.Svc.Carries(ctx, f.Person.ID, f.Conversation, accepted.TurnID); err != nil || carries {
		t.Fatalf("a person who lost Worklet: Carries = %v, %v; want false", carries, err)
	}
	rounds := len(f.Model.Requests())
	at, err := f.Svc.Resume(ctx, approval.Resumption{Owner: f.Person.ID,
		Conversation: f.Conversation, Turn: accepted.TurnID, Proposal: p.ID,
		Read: f.Grant("decline"), State: wire.ProposalDeclined,
		Card: cardAt(p, wire.ProposalDeclined)})
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, at.AfterSeq)
	if n := len(f.Model.Requests()); n != rounds {
		t.Errorf("the model was asked %d more rounds for a person who lost Worklet", n-rounds)
	}
	failure := evals.Last(frames, wire.EventError)
	done := evals.Last(frames, wire.EventDone)
	if failure == nil || failure.Error.Code != wire.ErrorAccessWithdrawn ||
		done == nil || done.Done.State != wire.TurnFailed {
		t.Errorf("the turn ended %+v with %+v, want failed, access withdrawn", done, failure)
	}
	row, _ := f.Store.Turn(t.Context(), f.Person.ID, accepted.TurnID)
	if row.State != wire.TurnFailed || row.ErrorCode != string(wire.ErrorAccessWithdrawn) {
		t.Errorf("the turn is %s (%s), want failed, access withdrawn", row.State, row.ErrorCode)
	}
	if !f.Revoker.Revoked(f.Grant("decline").ID()) {
		t.Error("the decision's grant was left live with no runner to take it")
	}
}

// ANOTHER PERSON ASKING WHETHER A TURN CARRIES ON IS TOLD NOTHING.
func TestCarriesIsTheOwnersQuestion(t *testing.T) {
	t.Parallel()
	f, accepted, _ := readThenProposeRig(t)
	ada := evals.Person("ada", "ada", iam.GrantWorklet)
	_, err := f.Svc.Carries(iam.WithPrincipal(context.Background(), ada), f.Person.ID,
		f.Conversation, accepted.TurnID)
	if !errors.Is(err, assistant.ErrNotFound) {
		t.Errorf("ada asking about grace's turn: err = %v, want not found", err)
	}
}
