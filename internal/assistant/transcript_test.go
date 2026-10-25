package assistant_test

import (
	"context"
	"maps"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/iam"
)

// view is what the drawer shows of a conversation's turns, reduced to what a
// reload must agree with: which message each card hangs on, the state each
// step ended in, and every assistant message's text.
type view struct {
	cards    map[uuid.UUID]uuid.UUID
	steps    map[uuid.UUID]wire.StepState
	messages map[uuid.UUID]string
}

func newView() view {
	return view{cards: map[uuid.UUID]uuid.UUID{}, steps: map[uuid.UUID]wire.StepState{},
		messages: map[uuid.UUID]string{}}
}

// liveView is the view the frames drew, as the drawer folds them.
func liveView(frames []wire.Frame) view {
	v := newView()
	for _, f := range frames {
		switch f.Kind {
		case wire.EventDelta:
			if f.Delta.Restart {
				v.messages[f.Delta.MessageID] = f.Delta.Text
			} else {
				v.messages[f.Delta.MessageID] += f.Delta.Text
			}
		case wire.EventCard:
			v.cards[f.Card.CardID] = f.Card.MessageID
		case wire.EventStep:
			v.steps[f.Step.StepID] = f.Step.State
		case wire.EventSnapshot:
			for _, m := range f.Snapshot.Messages {
				if m.Role == wire.RoleAssistant {
					v.messages[m.ID] = m.Text
				}
			}
			for _, c := range f.Snapshot.Cards {
				v.cards[c.CardID] = c.MessageID
			}
			for _, st := range f.Snapshot.Steps {
				v.steps[st.StepID] = st.State
			}
		}
	}
	return v
}

// storedView is the view a reload of the transcript draws.
func storedView(t *testing.T, f *assistanttest.Rig) view {
	t.Helper()
	got, err := f.Svc.Transcript(t.Context(), f.Person, f.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	v := newView()
	for _, m := range got.Messages {
		if m.Role == wire.RoleAssistant {
			v.messages[m.ID] = m.Text
		}
	}
	for _, c := range got.Cards {
		v.cards[c.CardID] = c.MessageID
	}
	for _, st := range got.Steps {
		v.steps[st.StepID] = st.State
	}
	return v
}

// agree fails where a reload would draw the turn otherwise than it was drawn
// live, or a stored card hangs on a message the transcript does not hold.
func agree(t *testing.T, live, stored view) {
	t.Helper()
	for card, message := range stored.cards {
		if _, ok := stored.messages[message]; !ok {
			t.Errorf("card %s hangs on message %s, which the transcript does not hold: a "+
				"reload drops it", card, message)
		}
	}
	if !maps.Equal(live.cards, stored.cards) {
		t.Errorf("cards live %v, reloaded %v", live.cards, stored.cards)
	}
	if !maps.Equal(live.steps, stored.steps) {
		t.Errorf("steps live %v, reloaded %v", live.steps, stored.steps)
	}
	for id, text := range live.messages {
		if text != "" && stored.messages[id] != text {
			t.Errorf("message %s read %q live and %q reloaded", id, text, stored.messages[id])
		}
	}
}

// A RELOAD DRAWS THE TURN AS IT WAS DRAWN LIVE (QA E7): a card from a segment
// that wrote no prose hangs on a message the transcript holds, and the step a
// parked turn waited on ends in what became of its proposal, live and
// reloaded alike, never "waiting" once the turn is over.
//
// Mutation: record a segment's answer only when it has prose and the parked
// card vanishes on reload; drop the step's settlement and it reads waiting
// after the turn completed.
func TestAReloadDrawsTheTurnAsItWasDrawnLive(t *testing.T) {
	t.Parallel()
	f, accepted, p := parkedRig(t)
	parked := f.Follow(accepted.TurnID, 0)
	agree(t, liveView(parked), storedView(t, f))

	ctx := iam.WithPrincipal(context.Background(), f.Person)
	resumed, err := f.Svc.Resume(ctx, approval.Resumption{Owner: f.Person.ID,
		Conversation: f.Conversation, Turn: accepted.TurnID, Proposal: p.ID,
		Read: f.Grant("approve"), State: wire.ProposalApplied,
		Receipt: &wire.Receipt{Summary: "Created ENG-7."}, Card: cardAt(p, wire.ProposalApplied)})
	if err != nil {
		t.Fatal(err)
	}
	frames := append(parked, f.Follow(accepted.TurnID, resumed.AfterSeq)...)
	live := liveView(frames)
	for step, state := range live.steps {
		if state == wire.StepWaiting {
			t.Errorf("step %s still reads waiting after the turn completed", step)
		}
	}
	agree(t, live, storedView(t, f))
}

// A STOP OF A PARKED TURN, AND ITS EXPIRY, SETTLE THE STEP IT WAITED ON: a
// reload of a turn that is over never shows a step waiting.
func TestAParkedTurnsEndSettlesTheStepItWaitedOn(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]func(*testing.T, *assistanttest.Rig, uuid.UUID){
		"stop": func(t *testing.T, f *assistanttest.Rig, turn uuid.UUID) {
			ctx := iam.WithPrincipal(context.Background(), f.Person)
			if _, err := f.Svc.Stop(ctx, f.Person.ID, turn, "stop"); err != nil {
				t.Fatal(err)
			}
		},
		"expiry": func(t *testing.T, f *assistanttest.Rig, _ uuid.UUID) {
			p := f.Store.Proposals()[0]
			pastExpiry(f, p)
			if _, err := f.Svc.ReapLapsed(t.Context()); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f, accepted, _ := parkedRig(t)
			end(t, f, accepted.TurnID)
			for step, state := range storedView(t, f).steps {
				if state == wire.StepWaiting {
					t.Errorf("after the %s, step %s still reads waiting", name, step)
				}
			}
		})
	}
}
