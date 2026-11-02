package assistant_test

import (
	"context"
	"maps"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
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

// order is a turn's answer as the drawer lays it out: each assistant message's
// text, followed by the cards that hang on it, in the order drawn.
func order(messages []uuid.UUID, text map[uuid.UUID]string, cards [][2]uuid.UUID) []string {
	var out []string
	for _, m := range messages {
		if text[m] != "" {
			out = append(out, "prose:"+text[m])
		}
		for _, c := range cards {
			if c[1] == m {
				out = append(out, "card:"+c[0].String())
			}
		}
	}
	return out
}

// EACH ROUND IS ITS OWN MESSAGE, LIVE AND RELOADED (QA E8): a round's prose
// sits above the cards its calls showed and the final round's prose below
// them, every round a paragraph of its own, and a reload lays the turn out
// exactly as it was drawn.
//
// Mutation: join the rounds into one message and the final sentence renders
// above the card it follows, with no break where the rounds met.
func TestEachRoundsProseIsItsOwnMessageLiveAndReloaded(t *testing.T) {
	t.Parallel()
	look := readTask()
	look.Text = "Let me look that task up."
	f := assistanttest.NewRig(t, evals.RigOptions{Tools: []catalog.Tool{getTask(t)}, Handler: task,
		Rounds: []evals.Round{look, evals.Say("ENG-7 rotates the signing keys.")}})
	accepted, err := f.Ask("rounds", "What is ENG-7?", nil)
	if err != nil {
		t.Fatal(err)
	}
	frames := f.Follow(accepted.TurnID, 0)
	var drawnOrder []uuid.UUID
	drawnText := map[uuid.UUID]string{}
	var drawnCards [][2]uuid.UUID
	for _, fr := range frames {
		switch fr.Kind {
		case wire.EventDelta:
			id := fr.Delta.MessageID
			if _, seen := drawnText[id]; !seen {
				drawnOrder = append(drawnOrder, id)
			}
			if fr.Delta.Restart {
				drawnText[id] = fr.Delta.Text
			} else {
				drawnText[id] += fr.Delta.Text
			}
		case wire.EventCard:
			drawnCards = append(drawnCards, [2]uuid.UUID{fr.Card.CardID, fr.Card.MessageID})
		}
	}
	live := order(drawnOrder, drawnText, drawnCards)
	want := []string{"prose:Let me look that task up.", "card:" + drawnCards[0][0].String(),
		"prose:ENG-7 rotates the signing keys."}
	if !slices.Equal(live, want) {
		t.Fatalf("drawn live as %q, want %q", live, want)
	}

	got, err := f.Svc.Transcript(t.Context(), f.Person, f.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	var storedOrder []uuid.UUID
	storedText := map[uuid.UUID]string{}
	for _, m := range got.Messages {
		if m.Role == wire.RoleAssistant {
			storedOrder = append(storedOrder, m.ID)
			storedText[m.ID] = m.Text
		}
	}
	var storedCards [][2]uuid.UUID
	for _, c := range got.Cards {
		storedCards = append(storedCards, [2]uuid.UUID{c.CardID, c.MessageID})
	}
	if reloaded := order(storedOrder, storedText, storedCards); !slices.Equal(reloaded, live) {
		t.Errorf("reloaded as %q, drawn live as %q", reloaded, live)
	}
}
