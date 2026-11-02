package assistant_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// A CONVERSATION IS NAMED BY ITS FIRST QUESTION AT ONCE, whatever its first
// turn does: the measured history listed a conversation whose first turn was
// stopped, and one whose first turn parked on a change, as "Untitled
// conversation", because only a first answer that completed was ever titled.
//
// Mutation: title only a completed first answer again, and the stopped
// conversation stays untitled.
func TestAConversationIsNamedByItsFirstQuestionAtOnce(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{evals.Say("Hi.")}})
	f.Model.Gate = make(chan struct{})
	accepted, err := f.Ask("first", "  please add a new agent role\nfor me  ", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Svc.Stop(t.Context(), f.Person.ID, accepted.TurnID, evals.Key("stop")); err != nil {
		t.Fatal(err)
	}
	if done := evals.Last(f.Follow(accepted.TurnID, 0), wire.EventDone); done == nil ||
		done.Done.State != wire.TurnStopped {
		t.Fatalf("the first turn did not end stopped: %+v", done)
	}
	c, err := f.Store.Conversation(t.Context(), f.Person.ID, f.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	if c.Title != "please add a new agent role for me" {
		t.Errorf("title = %q, want the first question on one line", c.Title)
	}

	// A LATER QUESTION DOES NOT RENAME IT.
	close(f.Model.Gate)
	second, err := f.Ask("second", "and another thing", nil)
	if err != nil {
		t.Fatal(err)
	}
	evals.Last(f.Follow(second.TurnID, 0), wire.EventDone)
	if c, _ = f.Store.Conversation(t.Context(), f.Person.ID, f.Conversation); c.Title !=
		"please add a new agent role for me" {
		t.Errorf("a second question renamed the conversation to %q", c.Title)
	}
}

// The first question's title is redacted and bounded like the model's.
func TestAQuestionTitleIsRedactedAndBounded(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{Rounds: []evals.Round{evals.Say("Done."),
		evals.Say("Rotating the key")}})
	f.Model.Gate = make(chan struct{})
	question := "rotate the key sk-ant-api03-" + strings.Repeat("A", 40) +
		" and then explain to me in great detail every single step that the rotation takes"
	accepted, err := f.Ask("long", question, nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := f.Store.Conversation(t.Context(), f.Person.ID, f.Conversation)
	if err != nil {
		t.Fatal(err)
	}
	close(f.Model.Gate)
	evals.Last(f.Follow(accepted.TurnID, 0), wire.EventDone)
	if strings.Contains(c.Title, "AAAAAAAA") {
		t.Errorf("the title carries the key: %q", c.Title)
	}
	if n := utf8.RuneCountInString(c.Title); n > assistant.TitleMaxRunes || !strings.HasSuffix(c.Title, "…") {
		t.Errorf("title = %q (%d runes), want at most %d, marked as cut", c.Title, n, assistant.TitleMaxRunes)
	}
}
